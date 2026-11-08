package models

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/samber/lo"
	"gorm.io/gorm"
)

func TestInit_BackfillsAuthKeyIOLogToFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmio.db")

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open seed database: %v", err)
	}

	if err := db.Exec(`
		CREATE TABLE auth_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME,
			name TEXT,
			key TEXT,
			status NUMERIC,
			allow_all NUMERIC,
			models TEXT,
			expires_at DATETIME,
			usage_count INTEGER,
			last_used_at DATETIME
		)
	`).Error; err != nil {
		t.Fatalf("failed to create legacy auth_keys table: %v", err)
	}

	if err := db.Exec(`INSERT INTO auth_keys (name, key, status, allow_all, usage_count) VALUES (?, ?, ?, ?, ?)`,
		"legacy-project", "legacy-key", true, true, 0,
	).Error; err != nil {
		t.Fatalf("failed to seed legacy auth key: %v", err)
	}

	Init(context.Background(), path)

	authKey, err := gorm.G[AuthKey](DB).Where("key = ?", "legacy-key").First(context.Background())
	if err != nil {
		t.Fatalf("failed to load migrated auth key: %v", err)
	}

	if authKey.IOLog == nil {
		t.Fatal("expected io_log to be backfilled")
	}
	if *authKey.IOLog {
		t.Fatal("expected io_log default to false")
	}
}

func TestInit_BackfillsPreferredProviderToPriorities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmio.db")

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open seed database: %v", err)
	}

	if err := db.Exec(`
		CREATE TABLE auth_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME,
			name TEXT,
			key TEXT,
			status NUMERIC,
			preferred_provider_id INTEGER,
			allow_all NUMERIC,
			models TEXT,
			expires_at DATETIME,
			usage_count INTEGER,
			last_used_at DATETIME
		)
	`).Error; err != nil {
		t.Fatalf("failed to create legacy auth_keys table: %v", err)
	}

	if err := db.Exec(`INSERT INTO auth_keys (name, key, status, preferred_provider_id, allow_all, usage_count) VALUES
		(?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?),
		(?, ?, ?, ?, ?, ?)`,
		"with-preferred", "key-a", true, 5, true, 0,
		"no-preferred", "key-b", true, nil, true, 0,
		"zero-preferred", "key-c", true, 0, true, 0,
	).Error; err != nil {
		t.Fatalf("failed to seed legacy auth keys: %v", err)
	}

	Init(context.Background(), path)

	ctx := context.Background()

	migrated, err := gorm.G[AuthKey](DB).Where("key = ?", "key-a").First(ctx)
	if err != nil {
		t.Fatalf("failed to load migrated auth key: %v", err)
	}
	if len(migrated.ProviderPriorities) != 1 || migrated.ProviderPriorities[0] != 5 {
		t.Fatalf("expected provider priorities [5], got %v", migrated.ProviderPriorities)
	}
	if migrated.PreferredProviderID != nil {
		t.Fatalf("expected preferred_provider_id cleared after migration, got %v", *migrated.PreferredProviderID)
	}

	untouched, err := gorm.G[AuthKey](DB).Where("key = ?", "key-b").First(ctx)
	if err != nil {
		t.Fatalf("failed to load key-b: %v", err)
	}
	if len(untouched.ProviderPriorities) != 0 {
		t.Fatalf("expected empty provider priorities for key-b, got %v", untouched.ProviderPriorities)
	}

	zero, err := gorm.G[AuthKey](DB).Where("key = ?", "key-c").First(ctx)
	if err != nil {
		t.Fatalf("failed to load key-c: %v", err)
	}
	if len(zero.ProviderPriorities) != 0 {
		t.Fatalf("expected zero preferred not to produce a list, got %v", zero.ProviderPriorities)
	}
}

func TestEnsureAuthKeyProviderPriorities_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llmio.db")
	Init(context.Background(), path)

	ctx := context.Background()

	seeded := AuthKey{Name: "seed", Key: "seed-key", Status: lo.ToPtr(true), AllowAll: lo.ToPtr(true)}
	if err := gorm.G[AuthKey](DB).Create(ctx, &seeded); err != nil {
		t.Fatalf("failed to create seed auth key: %v", err)
	}

	// 模拟已由旧字段迁移过一次：列表有值，但 preferred 也残留非空——已有列表不应被覆盖
	if _, err := gorm.G[AuthKey](DB).Where("id = ?", seeded.ID).
		Select("provider_priorities", "preferred_provider_id").
		Updates(ctx, AuthKey{
			ProviderPriorities:  []uint{7},
			PreferredProviderID: lo.ToPtr(uint(5)),
		}); err != nil {
		t.Fatalf("failed to seed conflicting state: %v", err)
	}

	if err := ensureAuthKeyProviderPriorities(ctx); err != nil {
		t.Fatalf("ensureAuthKeyProviderPriorities failed: %v", err)
	}

	after, err := gorm.G[AuthKey](DB).Where("id = ?", seeded.ID).First(ctx)
	if err != nil {
		t.Fatalf("failed to reload auth key: %v", err)
	}
	if len(after.ProviderPriorities) != 1 || after.ProviderPriorities[0] != 7 {
		t.Fatalf("expected existing list [7] preserved, got %v", after.ProviderPriorities)
	}

	// 模拟用户清空列表后重启：preferred 已在迁移时清空，再次执行不应让旧值复活
	if _, err := gorm.G[AuthKey](DB).Where("id = ?", seeded.ID).
		Select("provider_priorities", "preferred_provider_id").
		Updates(ctx, AuthKey{
			ProviderPriorities:  []uint{},
			PreferredProviderID: lo.ToPtr(uint(5)),
		}); err != nil {
		t.Fatalf("failed to reset state: %v", err)
	}
	if err := ensureAuthKeyProviderPriorities(ctx); err != nil {
		t.Fatalf("second ensureAuthKeyProviderPriorities failed: %v", err)
	}
	final, err := gorm.G[AuthKey](DB).Where("id = ?", seeded.ID).First(ctx)
	if err != nil {
		t.Fatalf("failed to reload auth key after second run: %v", err)
	}

	// preferred 非空 + 列表为空 → 属于待迁移状态，按定义重新迁移为 [preferred]（数据表达了明确意图）
	if len(final.ProviderPriorities) != 1 || final.ProviderPriorities[0] != 5 {
		t.Fatalf("expected re-migration to [5] for preferred=5 with empty list, got %v", final.ProviderPriorities)
	}
	if final.PreferredProviderID != nil {
		t.Fatalf("expected preferred cleared after re-migration, got %v", *final.PreferredProviderID)
	}
}
