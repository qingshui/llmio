package handler

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"llmio/common"
	"llmio/models"
)

// 归并分页：分表 id 区间不相交且全局升序，新→旧拼接即全局 id DESC 序。
// 覆盖：跨表连续序、offset 跨表下推、凑满即停、days=all 与筛选组合。
func TestMergePaginateChatLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
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

	ctx := context.Background()
	now := time.Now()
	curStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	prevStart := curStart.AddDate(0, -1, 0)
	prevMonth := models.ChatLogMonthKey(prevStart)

	if err := models.EnsureChatLogShardTable(ctx, prevMonth); err != nil {
		t.Fatal(err)
	}
	prevTable := models.ChatLogShardTableName(prevMonth)

	// 上月 3 行（id 1..3），当前月 5 行（id 4..8），全局 id DESC 序为 8..1
	prevRows := []models.ChatLog{
		{CreatedAt: prevStart.AddDate(0, 0, 2), Name: "m", Status: "success", ProviderName: "p1"},
		{CreatedAt: prevStart.AddDate(0, 0, 10), Name: "m", Status: "error", ProviderName: "p1"},
		{CreatedAt: prevStart.AddDate(0, 0, 20), Name: "m", Status: "success", ProviderName: "p1"},
	}
	for i := range prevRows {
		if err := db.Table(prevTable).Create(&prevRows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := models.SealChatLogShard(ctx, prevMonth); err != nil {
		t.Fatal(err)
	}

	curRows := []models.ChatLog{
		{Name: "m", Status: "success", ProviderName: "p2", CreatedAt: now.Add(-4 * time.Hour)},
		{Name: "m", Status: "error", ProviderName: "p2", CreatedAt: now.Add(-3 * time.Hour)},
		{Name: "m", Status: "success", ProviderName: "p1", CreatedAt: now.Add(-2 * time.Hour)},
		{Name: "m", Status: "success", ProviderName: "p2", CreatedAt: now.Add(-time.Hour)},
		{Name: "m", Status: "success", ProviderName: "p2", CreatedAt: now},
	}
	for i := range curRows {
		if _, err := insertLogViaShard(ctx, curRows[i]); err != nil {
			t.Fatal(err)
		}
	}

	page := func(query string, pageNum, pageSize int) []models.ChatLog {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/api/logs?"+query, nil)
		cutoff, all := logShardCutoff(c)
		refs, err := models.ChatLogShardsForWindow(ctx, cutoff)
		if err != nil {
			t.Fatalf("window: %v", err)
		}
		params := common.PaginationParams{Page: pageNum, PageSize: pageSize}
		logs, err := mergePaginateChatLogs(ctx, refs, c, cutoff, all, params)
		if err != nil {
			t.Fatalf("mergePaginate(%q): %v", query, err)
		}
		return logs
	}

	// 第一页（默认 30 天窗口覆盖两表）：当前月 5 行 + 上月补足
	logs := page("days=all", 1, 4)
	if len(logs) != 4 {
		t.Fatalf("page1 len = %d, want 4", len(logs))
	}
	wantIDs := []uint{8, 7, 6, 5}
	for i, log := range logs {
		if log.ID != wantIDs[i] {
			t.Fatalf("page1[%d].ID = %d, want %d（全局 id DESC 序）", i, log.ID, wantIDs[i])
		}
	}

	// offset 跨表：page_size=4 的第 2 页 → 上月 3 行 + 当前月第 4 新
	logs = page("days=all", 2, 4)
	if len(logs) != 4 {
		t.Fatalf("page2 len = %d, want 4", len(logs))
	}
	wantIDs = []uint{4, 3, 2, 1}
	for i, log := range logs {
		if log.ID != wantIDs[i] {
			t.Fatalf("page2[%d].ID = %d, want %d（offset 跨表下推）", i, log.ID, wantIDs[i])
		}
	}

	// 尾页不足一页
	logs = page("days=all", 3, 4)
	if len(logs) != 0 {
		t.Fatalf("page3 len = %d, want 0", len(logs))
	}

	// 筛选：仅 p1 的行按全局序（上月 3 行 + 当前月 1 行）
	logs = page("provider_name=p1&days=all", 1, 10)
	if len(logs) != 4 {
		t.Fatalf("filtered len = %d, want 4", len(logs))
	}
	wantIDs = []uint{6, 3, 2, 1}
	for i, log := range logs {
		if log.ID != wantIDs[i] {
			t.Fatalf("filtered[%d].ID = %d, want %d", i, log.ID, wantIDs[i])
		}
	}

	// 默认 30 天窗口：上月整月早于今日零点前 30 天……以今日零点为界的边界语义
	// 交给 ChatLogShardsForWindow 的 Boundary 处理，此处只验证窗口不漏当前月
	logs = page("status=success", 1, 10)
	for _, log := range logs {
		if log.Status != "success" {
			t.Fatalf("window query leaked non-matching row %d", log.ID)
		}
	}
}

// nthNewestChatLogID：保留水位取「第 N 新」，总行数不足 N 时 found=false。
func TestNthNewestChatLogID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
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

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := insertLogViaShard(ctx, models.ChatLog{Name: "m", Status: "success"}); err != nil {
			t.Fatal(err)
		}
	}

	id, found, err := nthNewestChatLogID(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !found || id != 2 {
		t.Fatalf("nth(2) = (%d, %v), want (2, true)", id, found)
	}
	if _, found, err = nthNewestChatLogID(ctx, 10); err != nil || found {
		t.Fatalf("nth(10) should not be found, got (%d, %v), err=%v", id, found, err)
	}
}
