package models

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"llmio/consts"
	"llmio/pkg/env"
)

var DB *gorm.DB

func Init(ctx context.Context, path string) {
	driver := env.GetWithDefault("DB_DRIVER", "sqlite")
	if driver == "sqlite" {
		if err := ensureDBFile(path); err != nil {
			panic(err)
		}
	}
	db, err := openDB(driver, path)
	if err != nil {
		panic(err)
	}
	DB = db
	if err := db.AutoMigrate(
		&Provider{},
		&Model{},
		&ModelWithProvider{},
		&ChatLog{},
		&ChatIO{},
		&Config{},
		&AuthKey{},
		&LogCleanupRecord{},
	); err != nil {
		panic(err)
	}
	// 兼容性考虑
	if _, err := gorm.G[ModelWithProvider](DB).Where("status IS NULL").Update(ctx, "status", true); err != nil {
		panic(err)
	}
	if _, err := gorm.G[ModelWithProvider](DB).Where("customer_headers IS NULL").Updates(ctx, ModelWithProvider{
		CustomerHeaders: map[string]string{},
	}); err != nil {
		panic(err)
	}
	if _, err := gorm.G[Model](DB).Where("strategy = '' OR strategy IS NULL").Update(ctx, "strategy", consts.BalancerDefault); err != nil {
		panic(err)
	}
	if err := ensureModelDisplayOrder(ctx); err != nil {
		panic(err)
	}
	if _, err := gorm.G[Model](DB).Where("breaker IS NULL").Update(ctx, "breaker", false); err != nil {
		panic(err)
	}
	if _, err := gorm.G[AuthKey](DB).Where("io_log IS NULL").Update(ctx, "io_log", false); err != nil {
		panic(err)
	}
	if _, err := gorm.G[ChatLog](DB).Where("auth_key_id IS NULL").Update(ctx, "auth_key_id", 0); err != nil {
		panic(err)
	}
	if err := ensureLogCleanupPolicyConfig(ctx); err != nil {
		panic(err)
	}
	zero := 0.0
	if _, err := gorm.G[ModelWithProvider](DB).Where("input_price IS NULL").Update(ctx, "input_price", &zero); err != nil {
		panic(err)
	}
	if _, err := gorm.G[ModelWithProvider](DB).Where("cache_read_price IS NULL").Update(ctx, "cache_read_price", &zero); err != nil {
		panic(err)
	}
	if _, err := gorm.G[ModelWithProvider](DB).Where("output_price IS NULL").Update(ctx, "output_price", &zero); err != nil {
		panic(err)
	}
	if _, err := gorm.G[ModelWithProvider](DB).Where("currency = '' OR currency IS NULL").Update(ctx, "currency", "CNY"); err != nil {
		panic(err)
	}

	if driver == "sqlite" && env.GetWithDefault("DB_VACUUM", false) {
		// 启动时执行 VACUUM 回收空间
		if err := db.Exec("VACUUM").Error; err != nil {
			panic(err)
		}
	}
}

func ensureModelDisplayOrder(ctx context.Context) error {
	needAssign, err := gorm.G[Model](DB).
		Where("display_order = 0 OR display_order IS NULL").
		Order("id ASC").
		Find(ctx)
	if err != nil {
		return err
	}
	if len(needAssign) == 0 {
		return nil
	}

	var currentMax int
	if err := DB.Model(&Model{}).
		Select("COALESCE(MAX(display_order), 0)").
		Scan(&currentMax).Error; err != nil {
		return err
	}

	return DB.Transaction(func(tx *gorm.DB) error {
		nextOrder := currentMax
		for _, model := range needAssign {
			nextOrder++
			if err := tx.WithContext(ctx).
				Model(&Model{}).
				Where("id = ?", model.ID).
				Update("display_order", nextOrder).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func ensureLogCleanupPolicyConfig(ctx context.Context) error {
	count, err := gorm.G[Config](DB).Where(&Config{Key: KeyLogCleanupPolicy}).Count(ctx, "*")
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	defaultLogCleanupPolicy, err := json.Marshal(LogCleanupPolicy{
		Enabled:       false,
		RetentionDays: 30,
	})
	if err != nil {
		return err
	}

	config := Config{
		Key:   KeyLogCleanupPolicy,
		Value: string(defaultLogCleanupPolicy),
	}
	if err := gorm.G[Config](DB).Create(ctx, &config); err != nil {
		return err
	}

	return nil
}

func ensureDBFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// openDB opens a GORM DB connection based on the driver name.
// driver="sqlite" (default): uses local file at path.
// driver="mysql": uses DSN from DATABASE_URL env var; path arg is ignored.
func openDB(driver, path string) (*gorm.DB, error) {
	switch driver {
	case "mysql":
		dsn := env.GetWithDefault("DATABASE_URL", "")
		if dsn == "" {
			return nil, fmt.Errorf("DATABASE_URL is required when DB_DRIVER=mysql")
		}
		return gorm.Open(gormmysql.Open(dsn))
	case "sqlite", "":
		return gorm.Open(sqlite.Open(path))
	default:
		return nil, fmt.Errorf("unsupported DB_DRIVER: %s (supported: sqlite, mysql)", driver)
	}
}
