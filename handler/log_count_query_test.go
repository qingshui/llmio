package handler

import (
	"net/http/httptest"
	"path/filepath"
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

func TestLogCountQueryForceIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mysqlDB := openDryRunDB(t, gormmysql.New(gormmysql.Config{
		DSN:                       "u:p@tcp(127.0.0.1:1)/db",
		SkipInitializeWithVersion: true,
	}))
	sqliteDB := openDryRunDB(t, sqlite.Open(":memory:"))

	tests := []struct {
		name  string
		db    *gorm.DB
		query string
		want  bool
	}{
		{"mysql auth key default 30d", mysqlDB, "auth_key_id=4", true},
		{"mysql auth key 90d", mysqlDB, "auth_key_id=4&days=90", true},
		{"mysql name", mysqlDB, "name=GLM-5.2", true},
		{"mysql name and auth key", mysqlDB, "name=GLM-5.2&auth_key_id=4", true},
		{"mysql days all skips", mysqlDB, "auth_key_id=4&days=all", false},
		{"mysql status skips", mysqlDB, "auth_key_id=4&status=success", false},
		{"mysql provider skips", mysqlDB, "provider_name=p1", false},
		{"mysql style skips", mysqlDB, "style=openai", false},
		{"sqlite never forces", sqliteDB, "auth_key_id=4", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/api/logs?"+tt.query, nil)

			var total int64
			sql := logCountQuery(tt.db, c, 0).Count(&total).Statement.SQL.String()

			if got := strings.Contains(sql, forceIndexSQL); got != tt.want {
				t.Errorf("force index = %v, want %v\nsql: %s", got, tt.want, sql)
			}
			if strings.Contains(sql, "deleted_at") {
				t.Errorf("count must be Unscoped, sql: %s", sql)
			}
		})
	}
}

func TestLogCountQueryIDBound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mysqlDB := openDryRunDB(t, gormmysql.New(gormmysql.Config{
		DSN:                       "u:p@tcp(127.0.0.1:1)/db",
		SkipInitializeWithVersion: true,
	}))

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/logs?auth_key_id=4&status=success", nil)

	var total int64
	sql := logCountQuery(mysqlDB, c, 5132113).Count(&total).Statement.SQL.String()

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

func TestCountLogsMatchesExactWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newDB := func() *gorm.DB {
		db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "logs.db")), &gorm.Config{})
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		if err := db.AutoMigrate(&models.ChatLog{}); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		return db
	}
	count := func(db *gorm.DB, query string) int64 {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/api/logs?"+query, nil)
		n, err := countLogs(db, c)
		if err != nil {
			t.Fatalf("countLogs(%q): %v", query, err)
		}
		return n
	}

	now := time.Now()
	day := 24 * time.Hour
	db := newDB()
	// 按时间顺序插入（id 随 created_at 单调），与线上追加写一致
	rows := []models.ChatLog{
		{CreatedAt: now.Add(-60 * day), Status: "success", ProviderName: "p1", Style: "openai"},
		{CreatedAt: now.Add(-40 * day), Status: "error", ProviderName: "p1", Style: "openai"},
		{CreatedAt: now.Add(-10 * day), Status: "success", ProviderName: "p1", Style: "anthropic"},
		{CreatedAt: now.Add(-5 * day), Status: "error", ProviderName: "p2", Style: "anthropic"},
		{CreatedAt: now.Add(-time.Hour), Status: "success", ProviderName: "p2", Style: "anthropic"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	tests := []struct {
		query string
		want  int64
	}{
		{"status=success", 2},
		{"status=error", 1},
		{"provider_name=p1", 1},
		{"style=anthropic&status=success", 2},
		{"status=success&days=90", 3},
		{"status=success&days=all", 3},
		{"status=error&days=all", 2},
		{"auth_key_id=0", 3},
	}
	for _, tt := range tests {
		if got := count(db, tt.query); got != tt.want {
			t.Errorf("countLogs(%q) = %d, want %d", tt.query, got, tt.want)
		}
	}

	empty := newDB()
	if err := empty.Create(&models.ChatLog{CreatedAt: now.Add(-60 * day), Status: "success"}).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := count(empty, "status=success"); got != 0 {
		t.Errorf("empty window: countLogs = %d, want 0", got)
	}
}
