package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"llmio/models"
)

// MigrateChatIOFromDB 是一次性迁移：把旧版存在 chat_ios 表 DB 列
// (input / of_string / of_string_array) 里的 ChatIO 数据搬到文件存储
// (logs/chat_io/<date>.log)，然后删除这三个孤儿列，使表结构与当前
// ChatIO 模型一致（模型现在只剩 LogId）。
//
// 注意：本迁移当前没有接线（无调用点），且其中的 LEFT JOIN chat_logs 针对
// 已废弃的 base 表——chat_logs 已按月分表（models/chat_log_shard.go），
// 活跃数据在 chat_logs_YYYYMM。仅当历史存量库尚未做过该迁移时才有意义，
// 分表化环境不要再接线执行。
//
// 背景：ChatIO 已从 DB 列存储改为按天 append-only 文件存储，但
// models.ChatIO 的 Input/OutputUnion 字段移除后，AutoMigrate 只加列不删列，
// 旧库仍残留这三个无人读写的列及其历史数据。
//
// 幂等：若 input 列已不存在，直接返回 nil；写入前先用 ReadChatIO 检查是否
// 已存在，避免崩溃重跑产生重复记录。
func MigrateChatIOFromDB(ctx context.Context) error {
	if !chatIOHasColumn("input") {
		return nil // 已迁移过
	}

	type legacyRow struct {
		LogID         uint
		Input         string
		OfString      string
		OfStringArray string
		CreatedAt     time.Time
	}
	var rows []legacyRow
	if err := models.DB.WithContext(ctx).
		Table("chat_ios").
		Select("chat_ios.log_id, chat_ios.input, chat_ios.of_string, chat_ios.of_string_array, chat_logs.created_at").
		Joins("LEFT JOIN chat_logs ON chat_logs.id = chat_ios.log_id").
		Scan(&rows).Error; err != nil {
		return fmt.Errorf("scan legacy chat_ios: %w", err)
	}

	migrated, skipped := 0, 0
	for _, r := range rows {
		hasInput := r.Input != ""
		hasOutput := r.OfString != "" || (r.OfStringArray != "" && r.OfStringArray != "[]" && r.OfStringArray != "null")
		if !hasInput && !hasOutput {
			skipped++
			continue
		}
		ts := r.CreatedAt
		if ts.IsZero() {
			ts = time.Now()
		}
		// 仅在文件存储里尚无对应记录时写入，保证部分写入后重跑不重复
		existingInput, existingOutput, _ := ReadChatIO(r.LogID, ts)
		if hasInput && existingInput == "" {
			if err := AppendChatIOInput(r.LogID, r.Input, ts); err != nil {
				return fmt.Errorf("append input for log %d: %w", r.LogID, err)
			}
		}
		if hasOutput && existingOutput == "" {
			var out models.OutputUnion
			if r.OfString != "" {
				out.OfString = r.OfString
			}
			if r.OfStringArray != "" && r.OfStringArray != "[]" && r.OfStringArray != "null" {
				if jerr := json.Unmarshal([]byte(r.OfStringArray), &out.OfStringArray); jerr != nil {
					// 解析失败退化为纯字符串，避免丢数据
					out.OfString = r.OfStringArray
				}
			}
			if err := AppendChatIOOutput(r.LogID, out, ts); err != nil {
				return fmt.Errorf("append output for log %d: %w", r.LogID, err)
			}
		}
		migrated++
	}

	// 数据已落盘后才删列；删列失败仅记录，下次启动重试（幂等）
	for _, col := range []string{"input", "of_string", "of_string_array"} {
		if !chatIOHasColumn(col) {
			continue
		}
		if err := models.DB.Migrator().DropColumn(&models.ChatIO{}, col); err != nil {
			return fmt.Errorf("drop column %s: %w", col, err)
		}
	}
	slog.Info("chat_ios legacy columns migrated & dropped", "rows", len(rows), "migrated", migrated, "skipped", skipped)
	return nil
}

// chatIOHasColumn 通过实际 DB 列元数据判断列是否存在。
// 不使用 Migrator().HasColumn 是因为它优先按 struct 字段名查找，而
// input/of_string/of_string_array 已不在 models.ChatIO 上，可能误判。
func chatIOHasColumn(name string) bool {
	cols, err := models.DB.Migrator().ColumnTypes(&models.ChatIO{})
	if err != nil {
		return false
	}
	for _, c := range cols {
		if c.Name() == name {
			return true
		}
	}
	return false
}
