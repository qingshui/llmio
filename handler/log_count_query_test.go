package handler

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"llmio/models"
)

const forceIndexSQL = "FORCE INDEX (idx_chat_logs_range_stats)"

func openDryRunDB(t *testing.T, dialector gorm.Dialector) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(dialector, &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("open dry-run db: %v", err)
	}
	return db
}

func newLogTestContext(query string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/logs?"+query, nil)
	return c
}

// 边界分表 + 覆盖索引内筛选 + MySQL → 强制 idx_chat_logs_range_stats；
// 整月覆盖分表 / days=all / 非覆盖列筛选 / sqlite 不强制。
func TestCountShardQueryForceIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mysqlDB := openDryRunDB(t, gormmysql.New(gormmysql.Config{
		DSN:                       "u:p@tcp(127.0.0.1:1)/db",
		SkipInitializeWithVersion: true,
	}))
	sqliteDB := openDryRunDB(t, sqlite.Open(":memory:"))

	boundaryRef := models.ChatLogShardRef{MonthKey: "202611", Table: "chat_logs_202611", Boundary: true}
	fullRef := models.ChatLogShardRef{MonthKey: "202611", Table: "chat_logs_202611", Boundary: false}
	cutoff := time.Now().AddDate(0, 0, -30)

	tests := []struct {
		name string
		db   *gorm.DB
		ref  models.ChatLogShardRef
		all  bool
		// minID 非零走 id 下界分支，与 FORCE INDEX 互斥
		minID         int64
		query         string
		wantForceIdx  bool
		wantCreatedAt bool
	}{
		{"mysql boundary auth key default 30d", mysqlDB, boundaryRef, false, 0, "auth_key_id=4", true, true},
		{"mysql boundary auth key 90d", mysqlDB, boundaryRef, false, 0, "auth_key_id=4&days=90", true, true},
		{"mysql boundary name", mysqlDB, boundaryRef, false, 0, "name=GLM-5.2", true, true},
		{"mysql full month never forces", mysqlDB, fullRef, false, 0, "auth_key_id=4", false, false},
		{"mysql days all boundary never forces", mysqlDB, boundaryRef, true, 0, "auth_key_id=4&days=all", false, false},
		{"mysql uncovered filter skips force", mysqlDB, boundaryRef, false, 0, "auth_key_id=4&status=success", false, true},
		{"mysql id bound skips force and time", mysqlDB, boundaryRef, false, 5132113, "auth_key_id=4&status=success", false, false},
		{"sqlite never forces", sqliteDB, boundaryRef, false, 0, "auth_key_id=4", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newLogTestContext(tt.query)
			var total int64
			sql := countShardQuery(tt.db, c, tt.ref, cutoff, tt.all, tt.minID).Count(&total).Statement.SQL.String()

			if got := strings.Contains(sql, forceIndexSQL); got != tt.wantForceIdx {
				t.Errorf("force index = %v, want %v\nsql: %s", got, tt.wantForceIdx, sql)
			}
			if got := strings.Contains(sql, "created_at >= ?"); got != tt.wantCreatedAt {
				t.Errorf("created_at predicate = %v, want %v\nsql: %s", got, tt.wantCreatedAt, sql)
			}
			if !strings.Contains(sql, "chat_logs_202611") {
				t.Errorf("sql should target shard table chat_logs_202611, sql: %s", sql)
			}
			if strings.Contains(sql, "deleted_at") {
				t.Errorf("count must be Unscoped, sql: %s", sql)
			}
		})
	}
}

func TestCountShardQueryIDBound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mysqlDB := openDryRunDB(t, gormmysql.New(gormmysql.Config{
		DSN:                       "u:p@tcp(127.0.0.1:1)/db",
		SkipInitializeWithVersion: true,
	}))

	c := newLogTestContext("auth_key_id=4&status=success")
	ref := models.ChatLogShardRef{MonthKey: "202611", Table: "chat_logs_202611", Boundary: true}

	var total int64
	sql := countShardQuery(mysqlDB, c, ref, time.Now().AddDate(0, 0, -30), false, 5132113).Count(&total).Statement.SQL.String()

	if !strings.Contains(sql, "id >= ?") {
		t.Errorf("want id lower bound, sql: %s", sql)
	}
	if strings.Contains(sql, "created_at") || strings.Contains(sql, forceIndexSQL) {
		t.Errorf("id-bound plan must not use created_at/force index, sql: %s", sql)
	}
	if strings.Contains(sql, "deleted_at") {
		t.Errorf("count must be Unscoped, sql: %s", sql)
	}
}

// 跨两张分表（上月封存 + 当前月）精确计数：期望值在测试内按窗口谓词镜像计算，
// 不依赖真实日历（跨月边界由月份起点显式构造）。
func TestFanoutCountLogsMatchesExactWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newDB := func(t *testing.T) *gorm.DB {
		db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "logs.db")), &gorm.Config{})
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		if err := db.AutoMigrate(&models.ChatLog{}, &models.ChatLogShard{}); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		oldDB := models.DB
		models.DB = db
		models.ResetChatLogShardState()
		t.Cleanup(func() {
			models.DB = oldDB
			models.ResetChatLogShardState()
		})
		return db
	}

	ctx := context.Background()
	now := time.Now()
	curStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	prevStart := curStart.AddDate(0, -1, 0)
	prevMonth := models.ChatLogMonthKey(prevStart)

	db := newDB(t)
	if err := models.EnsureChatLogShardTable(ctx, prevMonth); err != nil {
		t.Fatal(err)
	}

	prevTable := models.ChatLogShardTableName(prevMonth)
	rows := []models.ChatLog{
		// 上月（相对上月起点的天数，确保都落在上月内）
		{CreatedAt: prevStart.AddDate(0, 0, 2), Status: "success", ProviderName: "p1", Style: "openai"},
		{CreatedAt: prevStart.AddDate(0, 0, 20), Status: "error", ProviderName: "p1", Style: "openai"},
		// 当前月
		{CreatedAt: now.Add(-time.Hour), Status: "success", ProviderName: "p2", Style: "anthropic"},
		{CreatedAt: now, Status: "error", ProviderName: "p2", Style: "anthropic"},
	}
	// 前两行进上月分表，后两行进当前月分表（经生产写入路径，惰性建表）
	for i := range rows {
		if i < 2 {
			if err := db.Table(prevTable).Create(&rows[i]).Error; err != nil {
				t.Fatal(err)
			}
			continue
		}
		id, err := insertLogViaShard(ctx, rows[i])
		if err != nil {
			t.Fatal(err)
		}
		rows[i].ID = id
	}
	if err := models.SealChatLogShard(ctx, prevMonth); err != nil {
		t.Fatal(err)
	}

	count := func(query string) int64 {
		c := newLogTestContext(query)
		cutoff, all := logShardCutoff(c)
		refs, err := models.ChatLogShardsForWindow(ctx, cutoff)
		if err != nil {
			t.Fatalf("window(%q): %v", query, err)
		}
		n, err := fanoutCountLogs(ctx, models.DB, c, refs, cutoff, all)
		if err != nil {
			t.Fatalf("fanoutCountLogs(%q): %v", query, err)
		}
		return n
	}

	// 期望值按与查询相同的谓词镜像计算
	expect := func(windowDays int, match func(r models.ChatLog) bool) int64 {
		var n int64
		for _, r := range rows {
			if !match(r) {
				continue
			}
			if windowDays > 0 && r.CreatedAt.Before(now.AddDate(0, 0, -windowDays)) {
				continue
			}
			n++
		}
		return n
	}

	tests := []struct {
		query  string
		window int
		match  func(models.ChatLog) bool
	}{
		{"status=success", 30, func(r models.ChatLog) bool { return r.Status == "success" }},
		{"status=error", 30, func(r models.ChatLog) bool { return r.Status == "error" }},
		{"provider_name=p1", 30, func(r models.ChatLog) bool { return r.ProviderName == "p1" }},
		{"style=anthropic&status=success", 30, func(r models.ChatLog) bool { return r.Style == "anthropic" && r.Status == "success" }},
		{"status=success&days=90", 90, func(r models.ChatLog) bool { return r.Status == "success" }},
		{"status=success&days=all", 0, func(r models.ChatLog) bool { return r.Status == "success" }},
		{"status=error&days=all", 0, func(r models.ChatLog) bool { return r.Status == "error" }},
		{"auth_key_id=0&days=all", 0, func(r models.ChatLog) bool { return true }},
	}
	for _, tt := range tests {
		want := expect(tt.window, tt.match)
		if got := count(tt.query); got != want {
			t.Errorf("fanoutCountLogs(%q) = %d, want %d", tt.query, got, want)
		}
	}

	// id 点查：跨表路由到正确分表
	idQuery := func(id uint) int64 {
		c := newLogTestContext("id=" + strconv.FormatUint(uint64(id), 10) + "&days=all")
		cutoff, all := logShardCutoff(c)
		refs, err := models.ChatLogShardsForWindow(ctx, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		n, err := fanoutCountLogs(ctx, models.DB, c, refs, cutoff, all)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := idQuery(rows[0].ID); got != 1 {
		t.Errorf("id point count on sealed shard = %d, want 1", got)
	}
	if got := idQuery(rows[3].ID); got != 1 {
		t.Errorf("id point count on current shard = %d, want 1", got)
	}
	if got := idQuery(999999); got != 0 {
		t.Errorf("id point count missing = %d, want 0", got)
	}
}

// insertLogViaShard 经生产写入路径（分表路由）插入一行日志
func insertLogViaShard(ctx context.Context, log models.ChatLog) (uint, error) {
	err := models.WithChatLogShardForInsert(ctx, func(tx *gorm.DB, table string) error {
		return gorm.G[models.ChatLog](tx).Table(table).Create(ctx, &log)
	})
	return log.ID, err
}
