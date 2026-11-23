package models

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupShardTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "shards.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&ChatLog{}, &ChatLogShard{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	oldDB := DB
	DB = db
	t.Cleanup(func() {
		DB = oldDB
		ResetChatLogShardState()
		chatLogShardNow = time.Now
	})
	ResetChatLogShardState()
	return db
}

func insertShardLog(t *testing.T, name string) ChatLog {
	t.Helper()
	log := ChatLog{Name: name, Status: "success"}
	err := WithChatLogShardForInsert(context.Background(), func(tx *gorm.DB, table string) error {
		return gorm.G[ChatLog](tx).Table(table).Create(context.Background(), &log)
	})
	if err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
	return log
}

func TestChatLogShardNaming(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.Local)
	if got := ChatLogMonthKey(now); got != "202611" {
		t.Fatalf("ChatLogMonthKey = %q, want 202611", got)
	}
	if got := ChatLogShardTableName("202606"); got != "chat_logs_202606" {
		t.Fatalf("ChatLogShardTableName = %q", got)
	}
}

func TestChatLogShardInsertCreatesCurrentTable(t *testing.T) {
	setupShardTestDB(t)

	month := ChatLogMonthKey(time.Now())
	if err := ensureCurrentShard(context.Background(), month); err != nil {
		t.Fatalf("ensureCurrentShard: %v", err)
	}
	if !DB.Migrator().HasTable(ChatLogShardTableName(month)) {
		t.Fatalf("current shard table %s not created", ChatLogShardTableName(month))
	}
	var regCount int64
	if err := DB.Model(&ChatLogShard{}).Count(&regCount).Error; err != nil {
		t.Fatal(err)
	}
	if regCount != 0 {
		t.Fatalf("registry should be empty for current month, got %d rows", regCount)
	}

	log := insertShardLog(t, "cur")
	if log.ID == 0 {
		t.Fatal("insert should assign id")
	}
	var n int64
	if err := DB.Table(ChatLogShardTableName(month)).Where("id = ?", log.ID).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("row not found in current shard table, count=%d", n)
	}
}

func TestChatLogShardRolloverSealsPreviousMonth(t *testing.T) {
	setupShardTestDB(t)

	prevTime := time.Now().AddDate(0, -1, 0)
	chatLogShardNow = func() time.Time { return prevTime }
	prevLog := insertShardLog(t, "prev")
	chatLogShardNow = func() time.Time { return prevTime }
	prevLog2 := insertShardLog(t, "prev2")

	curTime := time.Now()
	chatLogShardNow = func() time.Time { return curTime }
	curLog := insertShardLog(t, "cur")

	prevMonth := ChatLogMonthKey(prevTime)
	curMonth := ChatLogMonthKey(curTime)

	// 上月封存：min/max/count 冻结，注册表恰好一行
	var shards []ChatLogShard
	if err := DB.Order("month_key ASC").Find(&shards).Error; err != nil {
		t.Fatal(err)
	}
	if len(shards) != 1 || shards[0].MonthKey != prevMonth {
		t.Fatalf("registry = %+v, want single row %s", shards, prevMonth)
	}
	if shards[0].MinID != prevLog.ID || shards[0].MaxID != prevLog2.ID || shards[0].RowsCount != 2 {
		t.Fatalf("sealed stats = %+v, want min=%d max=%d count=2", shards[0], prevLog.ID, prevLog2.ID)
	}

	// id 全局接续：当前月首个 id 紧接上月 max
	if curLog.ID != prevLog2.ID+1 {
		t.Fatalf("id continuation broken: prev max=%d, cur first=%d", prevLog2.ID, curLog.ID)
	}

	// LocateChatLogTable：封存区间命中 → 上月表；区间外 → 当前月表
	table, err := LocateChatLogTable(context.Background(), prevLog.ID)
	if err != nil || table != ChatLogShardTableName(prevMonth) {
		t.Fatalf("Locate(prev) = %q err=%v, want %q", table, err, ChatLogShardTableName(prevMonth))
	}
	table, err = LocateChatLogTable(context.Background(), curLog.ID)
	if err != nil || table != ChatLogShardTableName(curMonth) {
		t.Fatalf("Locate(cur) = %q err=%v, want %q", table, err, ChatLogShardTableName(curMonth))
	}

	// Probe：不存在的 id
	if _, err := ProbeChatLogTable(context.Background(), 999999); err != gorm.ErrRecordNotFound {
		t.Fatalf("Probe(missing) err = %v, want ErrRecordNotFound", err)
	}
}

func TestChatLogShardEmptyMonthSelfHeals(t *testing.T) {
	setupShardTestDB(t)

	emptyMonth := ChatLogMonthKey(time.Now().AddDate(0, -2, 0))
	// 手工建一个无行的非当前月分表（模拟该月无写入）
	table := ChatLogShardTableName(emptyMonth)
	if err := ensureShardTableLocked(context.Background(), emptyMonth); err != nil {
		t.Fatal(err)
	}
	if err := SealChatLogShard(context.Background(), emptyMonth); err != nil {
		t.Fatalf("SealChatLogShard(empty): %v", err)
	}
	// 空月不应注册，且空表被清理
	var regCount int64
	if err := DB.Model(&ChatLogShard{}).Count(&regCount).Error; err != nil {
		t.Fatal(err)
	}
	if regCount != 0 {
		t.Fatalf("empty month should not be registered, got %d", regCount)
	}
	if DB.Migrator().HasTable(table) {
		t.Fatalf("empty shard table %s should be dropped", table)
	}
}

func TestReconcileChatLogShardsConvergence(t *testing.T) {
	db := setupShardTestDB(t)
	ctx := context.Background()

	curMonth := ChatLogMonthKey(time.Now())
	prevTime := time.Now().AddDate(0, -1, 0)
	prevMonth := ChatLogMonthKey(prevTime)
	prevTable := ChatLogShardTableName(prevMonth)

	// 场景 1：有表无行（跨月宕机未封存）→ 对账补封存
	if err := ensureShardTableLocked(ctx, prevMonth); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := DB.Table(prevTable).Create(&ChatLog{Name: "x", Status: "success", CreatedAt: prevTime}).Error; err != nil {
			t.Fatal(err)
		}
	}
	// 场景 2：注册表有行但表不存在（DROP 后残留）→ 对账删行
	stale := ChatLogShard{MonthKey: "202501", MinID: 1, MaxID: 9, RowsCount: 9}
	if err := DB.Create(&stale).Error; err != nil {
		t.Fatal(err)
	}

	ResetChatLogShardState()
	if err := ReconcileChatLogShards(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var shards []ChatLogShard
	if err := DB.Order("month_key ASC").Find(&shards).Error; err != nil {
		t.Fatal(err)
	}
	if len(shards) != 1 || shards[0].MonthKey != prevMonth || shards[0].RowsCount != 3 {
		t.Fatalf("registry after reconcile = %+v, want single %s with 3 rows", shards, prevMonth)
	}
	if !db.Migrator().HasTable(ChatLogShardTableName(curMonth)) {
		t.Fatalf("current month table %s missing after reconcile", ChatLogShardTableName(curMonth))
	}

	// 对账后当前月分表自增接续（下一条 id = 上月 max + 1）
	chatLogShardNow = func() time.Time { return time.Now() }
	log := insertShardLog(t, "after")
	if log.ID != 4 {
		t.Fatalf("id after reconcile = %d, want 4", log.ID)
	}
}

func TestChatLogShardsForWindowCoverage(t *testing.T) {
	setupShardTestDB(t)
	ctx := context.Background()

	curMonth := ChatLogMonthKey(time.Now())
	prevTime := time.Now().AddDate(0, -1, 0)
	prevMonth := ChatLogMonthKey(prevTime)

	if err := ensureShardTableLocked(ctx, prevMonth); err != nil {
		t.Fatal(err)
	}
	if err := DB.Table(ChatLogShardTableName(prevMonth)).Create(&ChatLog{Name: "x", Status: "success", CreatedAt: prevTime}).Error; err != nil {
		t.Fatal(err)
	}
	if err := SealChatLogShard(ctx, prevMonth); err != nil {
		t.Fatal(err)
	}

	// 窗口不限时间：两表都整月覆盖，无 Boundary
	refs, err := ChatLogShardsForWindow(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("all-window refs = %+v, want 2", refs)
	}
	if refs[0].MonthKey != curMonth || refs[0].Boundary {
		t.Fatalf("refs[0] = %+v, want current month full coverage", refs[0])
	}
	if refs[1].MonthKey != prevMonth || refs[1].Boundary {
		t.Fatalf("refs[1] = %+v, want previous month full coverage", refs[1])
	}

	// cutoff 在本月内（今日零点）：仅当前月命中窗口（Boundary），整月早于
	// cutoff 的上月分表不进窗口
	today := time.Now()
	midnight := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.Local)
	refs, err = ChatLogShardsForWindow(ctx, midnight)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].MonthKey != curMonth || !refs[0].Boundary {
		t.Fatalf("today-window refs = %+v, want only boundary current month", refs)
	}

	// cutoff 在上月内：上月 Boundary + 当前月整月覆盖
	curStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.Local)
	prevStart := curStart.AddDate(0, -1, 0)
	cutoffInPrev := prevStart.AddDate(0, 0, 15)
	refs, err = ChatLogShardsForWindow(ctx, cutoffInPrev)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("prev-window refs = %+v, want 2", refs)
	}
	if refs[0].MonthKey != curMonth || refs[0].Boundary {
		t.Fatalf("current month should be fully covered, got %+v", refs[0])
	}
	if refs[1].MonthKey != prevMonth || !refs[1].Boundary {
		t.Fatalf("previous month should be boundary, got %+v", refs[1])
	}
}
