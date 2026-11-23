package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"llmio/models"
)

const (
	defaultLogRetentionDays = 30
	logCleanupCheckInterval = time.Hour
)

func DefaultLogCleanupPolicy() *models.LogCleanupPolicy {
	return &models.LogCleanupPolicy{
		Enabled:       false,
		RetentionDays: defaultLogRetentionDays,
	}
}

func GetLogCleanupPolicy(ctx context.Context) (*models.LogCleanupPolicy, error) {
	config, err := gorm.G[models.Config](models.DB).Where(&models.Config{Key: models.KeyLogCleanupPolicy}).First(ctx)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return DefaultLogCleanupPolicy(), nil
		}
		return nil, err
	}

	if config.Value == "" {
		return DefaultLogCleanupPolicy(), nil
	}

	policy := DefaultLogCleanupPolicy()
	if err := json.Unmarshal([]byte(config.Value), policy); err != nil {
		return nil, fmt.Errorf("unmarshal log cleanup policy: %w", err)
	}
	if policy.RetentionDays <= 0 {
		policy.RetentionDays = defaultLogRetentionDays
	}
	return policy, nil
}

func CleanLogsByDays(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, fmt.Errorf("days must be greater than 0")
	}

	cutoffTime := time.Now().AddDate(0, 0, -days)
	var deletedCount int64

	// 封存月：整月早于 cutoff 的分表直接 DROP（瞬时回收磁盘）；部分过期
	// 的边界月删行后重算封存统计。当前月同样按行删除。
	// DROP 必须在事务外执行：MariaDB 5.5 的 DDL 会隐式提交所在事务。
	for _, shard := range models.SealedChatLogShards() {
		start, err := models.ChatLogMonthStart(shard.MonthKey)
		if err != nil {
			return 0, err
		}
		switch {
		case !start.AddDate(0, 1, 0).After(cutoffTime):
			// 先按封存 id 区间级联删 chat_ios；崩溃中断由下次清理幂等重做
			if err := models.DB.WithContext(ctx).Unscoped().
				Where("log_id BETWEEN ? AND ?", shard.MinID, shard.MaxID).
				Delete(&models.ChatIO{}).Error; err != nil {
				return 0, fmt.Errorf("delete chat io of shard %s: %w", shard.MonthKey, err)
			}
			if err := models.DropChatLogShard(ctx, shard.MonthKey); err != nil {
				return 0, err
			}
			deletedCount += shard.RowsCount
		case cutoffTime.After(start):
			deleted, err := deleteChatLogsBefore(ctx, models.ChatLogShardTableName(shard.MonthKey), cutoffTime)
			if err != nil {
				return 0, err
			}
			deletedCount += deleted
			if err := models.SealChatLogShard(ctx, shard.MonthKey); err != nil {
				return 0, err
			}
		}
	}

	// 当前月分表内的过期行（cutoff 落在往月时不会命中任何行）
	deleted, err := deleteChatLogsBefore(ctx, models.CurrentChatLogShardTable(), cutoffTime)
	if err != nil {
		return 0, err
	}
	deletedCount += deleted

	// chat_io 文件按天存储，整日过期的日志文件直接整删；当天部分过期时
	// 文件保留（保留当天未过期的记录），等该日全部过期后再由后续清理删除。
	if removed, ferr := DeleteChatIOBeforeDate(cutoffTime); ferr != nil {
		slog.Error("delete chat_io files failed", "error", ferr)
	} else if removed > 0 {
		slog.Info("chat_io files removed", "count", removed)
	}

	// 清理删的是老行，总数水位线基数会虚高，必须失效重算
	InvalidateChatLogsWatermark(ctx)

	return deletedCount, nil
}

// deleteChatLogsBefore 在指定分表内删除 created_at 早于 cutoff 的行，
// 并同事务级联删除 chat_ios 关联行（表名由调用方给出，均来自内部生成）。
func deleteChatLogsBefore(ctx context.Context, table string, cutoff time.Time) (int64, error) {
	var deleted int64
	err := models.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().
			Where("log_id IN (SELECT id FROM `"+table+"` WHERE created_at < ?)", cutoff).
			Delete(&models.ChatIO{}).Error; err != nil {
			return err
		}
		result := tx.Unscoped().Table(table).Where("created_at < ?", cutoff).Delete(&models.ChatLog{})
		if result.Error != nil {
			return result.Error
		}
		deleted = result.RowsAffected
		return nil
	})
	return deleted, err
}

func StartLogCleanupScheduler(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(logCleanupCheckInterval)
		defer ticker.Stop()

		lastRunDay := ""
		run := func() {
			policy, err := GetLogCleanupPolicy(ctx)
			if err != nil {
				slog.Error("load log cleanup policy failed", "error", err)
				return
			}
			if !policy.Enabled || policy.RetentionDays <= 0 {
				return
			}

			today := time.Now().Format("2006-01-02")
			if lastRunDay == today {
				return
			}

			start := time.Now()
			deletedCount, err := CleanLogsByDays(ctx, policy.RetentionDays)
			if err != nil {
				slog.Error("scheduled log cleanup failed", "retention_days", policy.RetentionDays, "error", err)
				return
			}

			lastRunDay = today
			slog.Info("scheduled log cleanup completed",
				"retention_days", policy.RetentionDays,
				"deleted_count", deletedCount,
				"duration", time.Since(start),
			)

			record := models.LogCleanupRecord{
				RetentionDays: policy.RetentionDays,
				DeletedCount:  deletedCount,
				DurationMs:    time.Since(start).Milliseconds(),
				Source:        "scheduled",
				Type:          "days",
			}
			if err := gorm.G[models.LogCleanupRecord](models.DB).Create(ctx, &record); err != nil {
				slog.Error("failed to save cleanup record", "error", err)
			}
			models.TrimLogCleanupRecords(ctx, 100)
		}

		run()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}
