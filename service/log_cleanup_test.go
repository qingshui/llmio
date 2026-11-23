package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"llmio/models"
)

func setupLogCleanupTestDB(t *testing.T) func() {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}

	if err := db.AutoMigrate(&models.ChatLog{}, &models.ChatIO{}, &models.Config{}, &models.LogCleanupRecord{}, &models.ChatLogShard{}); err != nil {
		t.Fatalf("failed to migrate test database: %v", err)
	}

	models.DB = db
	models.ResetChatLogShardState()
	return func() {
		models.DB = nil
		models.ResetChatLogShardState()
	}
}

func TestCleanLogsByDays(t *testing.T) {
	cleanup := setupLogCleanupTestDB(t)
	defer cleanup()

	ctx := context.Background()
	shardTable := models.CurrentChatLogShardTable()

	oldLog := models.ChatLog{Name: "old-log", Status: "success"}
	oldID, err := SaveChatLog(ctx, oldLog)
	if err != nil {
		t.Fatalf("failed to create old log: %v", err)
	}
	if err := models.DB.Table(shardTable).Where("id = ?", oldID).Update("created_at", time.Now().AddDate(0, 0, -40)).Error; err != nil {
		t.Fatalf("failed to age old log: %v", err)
	}

	newLog := models.ChatLog{Name: "new-log", Status: "success"}
	newID, err := SaveChatLog(ctx, newLog)
	if err != nil {
		t.Fatalf("failed to create new log: %v", err)
	}
	if err := models.DB.Table(shardTable).Where("id = ?", newID).Update("created_at", time.Now().AddDate(0, 0, -2)).Error; err != nil {
		t.Fatalf("failed to date new log: %v", err)
	}

	if err := gorm.G[models.ChatIO](models.DB).Create(ctx, &models.ChatIO{LogId: oldID}); err != nil {
		t.Fatalf("failed to create old chat io: %v", err)
	}
	if err := gorm.G[models.ChatIO](models.DB).Create(ctx, &models.ChatIO{LogId: newID}); err != nil {
		t.Fatalf("failed to create new chat io: %v", err)
	}

	deletedCount, err := CleanLogsByDays(ctx, 30)
	if err != nil {
		t.Fatalf("CleanLogsByDays failed: %v", err)
	}
	if deletedCount != 1 {
		t.Fatalf("expected 1 deleted log, got %d", deletedCount)
	}

	var oldLogCount int64
	if err := models.DB.Unscoped().Table(shardTable).Where("id = ?", oldID).Count(&oldLogCount).Error; err != nil {
		t.Fatalf("failed to count old log: %v", err)
	}
	if oldLogCount != 0 {
		t.Fatalf("expected old log to be deleted, got count %d", oldLogCount)
	}

	var newLogCount int64
	if err := models.DB.Unscoped().Table(shardTable).Where("id = ?", newID).Count(&newLogCount).Error; err != nil {
		t.Fatalf("failed to count new log: %v", err)
	}
	if newLogCount != 1 {
		t.Fatalf("expected new log to remain, got count %d", newLogCount)
	}

	var oldChatIOCount int64
	if err := models.DB.Unscoped().Model(&models.ChatIO{}).Where("log_id = ?", oldID).Count(&oldChatIOCount).Error; err != nil {
		t.Fatalf("failed to count old chat io: %v", err)
	}
	if oldChatIOCount != 0 {
		t.Fatalf("expected old chat io to be deleted, got count %d", oldChatIOCount)
	}

	var newChatIOCount int64
	if err := models.DB.Unscoped().Model(&models.ChatIO{}).Where("log_id = ?", newID).Count(&newChatIOCount).Error; err != nil {
		t.Fatalf("failed to count new chat io: %v", err)
	}
	if newChatIOCount != 1 {
		t.Fatalf("expected new chat io to remain, got count %d", newChatIOCount)
	}
}

// 封存月整月早于 cutoff：DROP 分表 + 删注册表行 + 按 id 区间级联删 chat_ios
func TestCleanLogsByDaysDropsSealedMonth(t *testing.T) {
	cleanup := setupLogCleanupTestDB(t)
	defer cleanup()

	ctx := context.Background()

	// 两个月前的封存月：保证 cutoff 落在上月内时它整月早于 cutoff
	now := time.Now()
	curStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	prevStart := curStart.AddDate(0, -1, 0)
	prev2Key := models.ChatLogMonthKey(prevStart.AddDate(0, -1, 0))
	if err := models.EnsureChatLogShardTable(ctx, prev2Key); err != nil {
		t.Fatalf("ensure shard table: %v", err)
	}
	prev2Table := models.ChatLogShardTableName(prev2Key)

	ids := make([]uint, 0, 2)
	for _, name := range []string{"a", "b"} {
		row := models.ChatLog{Name: name, Status: "success"}
		if err := models.DB.Table(prev2Table).Create(&row).Error; err != nil {
			t.Fatalf("insert shard row: %v", err)
		}
		ids = append(ids, row.ID)
		if err := models.DB.Create(&models.ChatIO{LogId: row.ID}).Error; err != nil {
			t.Fatalf("insert chat io: %v", err)
		}
	}
	if err := models.SealChatLogShard(ctx, prev2Key); err != nil {
		t.Fatalf("seal shard: %v", err)
	}
	// 确保当前月分表存在（清理会向当前月表执行 DELETE）
	if err := models.ReconcileChatLogShards(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// cutoff 落在上月内：elapsed 天数回退（今天是 1 号时用 1 天，同样落在上月）
	days := int(now.Sub(prevStart).Hours() / 24)
	if days < 1 {
		days = 1
	}
	deleted, err := CleanLogsByDays(ctx, days)
	if err != nil {
		t.Fatalf("CleanLogsByDays failed: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 deleted rows, got %d", deleted)
	}
	if models.DB.Migrator().HasTable(prev2Table) {
		t.Fatalf("shard table %s should be dropped", prev2Table)
	}
	var regCount int64
	if err := models.DB.Model(&models.ChatLogShard{}).Count(&regCount).Error; err != nil {
		t.Fatal(err)
	}
	if regCount != 0 {
		t.Fatalf("registry should be empty after drop, got %d", regCount)
	}
	var ioCount int64
	if err := models.DB.Unscoped().Model(&models.ChatIO{}).Where("log_id IN ?", ids).Count(&ioCount).Error; err != nil {
		t.Fatal(err)
	}
	if ioCount != 0 {
		t.Fatalf("expected chat io rows to be cascade deleted, got %d", ioCount)
	}
}

func TestGetLogCleanupPolicyDefaultsWhenMissing(t *testing.T) {
	cleanup := setupLogCleanupTestDB(t)
	defer cleanup()

	policy, err := GetLogCleanupPolicy(context.Background())
	if err != nil {
		t.Fatalf("GetLogCleanupPolicy failed: %v", err)
	}
	if policy == nil {
		t.Fatal("expected policy pointer")
	}
	if policy.Enabled {
		t.Fatal("expected default policy to be disabled")
	}
	if policy.RetentionDays != defaultLogRetentionDays {
		t.Fatalf("expected default retention days %d, got %d", defaultLogRetentionDays, policy.RetentionDays)
	}
}

func TestGetLogCleanupPolicyReadsConfig(t *testing.T) {
	cleanup := setupLogCleanupTestDB(t)
	defer cleanup()

	value, err := json.Marshal(models.LogCleanupPolicy{
		Enabled:       true,
		RetentionDays: 7,
	})
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}

	if err := gorm.G[models.Config](models.DB).Create(context.Background(), &models.Config{
		Key:   models.KeyLogCleanupPolicy,
		Value: string(value),
	}); err != nil {
		t.Fatalf("failed to create config: %v", err)
	}

	policy, err := GetLogCleanupPolicy(context.Background())
	if err != nil {
		t.Fatalf("GetLogCleanupPolicy failed: %v", err)
	}
	if policy == nil {
		t.Fatal("expected policy pointer")
	}
	if !policy.Enabled {
		t.Fatal("expected policy to be enabled")
	}
	if policy.RetentionDays != 7 {
		t.Fatalf("expected retention days 7, got %d", policy.RetentionDays)
	}
}
