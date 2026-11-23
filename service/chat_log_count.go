package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"gorm.io/gorm"
	"llmio/models"
)

// chat_logs 总数 = Σ(封存月 rows_count，来自注册表 chat_log_shards) + 当前月分表行数。
// 当前月走「水位 + 增量」：base 为当前月分表内 (max_id, count)（id ≤ max_id 的行数），
// total = Σ封存 + count + COUNT(当前月 WHERE id > max_id)，主键只扫新增行，毫秒级且精确。
// 水位持久化在 config 表（v2，当前月口径），重启后增量续算无需重建；
// 月份切换封存后旧水位必然 ≤ 封存区间 max_id，据此判过期并触发后台重建。
// 日志清理删的是老行（id <= max_id），基数虚高无法增量纠正，因此清理后
// 失效水位并删除持久化记录；重建完成前用注册表合计 + 当前月估算值兜底。

type chatLogsWatermarkValue struct {
	MaxID int64 `json:"max_id"`
	Count int64 `json:"count"`
}

type chatLogsWatermark struct {
	mu         sync.Mutex
	loaded     bool
	valid      bool
	rebuilding bool
	maxID      int64
	count      int64
}

var chatLogsWM chatLogsWatermark

type chatLogsAggRow struct {
	Cnt   int64
	MaxID int64
}

// WarmChatLogsWatermark 启动时预热：恢复持久化水位或提前全量重建，
// 让重启后的首个请求就能拿到精确总数。
func WarmChatLogsWatermark() {
	wm := &chatLogsWM
	wm.mu.Lock()
	if !wm.loaded {
		wm.loadLocked(context.Background())
	}
	start := !wm.valid && !wm.rebuilding
	if start {
		wm.rebuilding = true
	}
	wm.mu.Unlock()
	if start {
		go rebuildChatLogsWatermark()
	}
}

// ChatLogsTotal 返回 chat_logs 总行数：水位有效时增量续算（精确，毫秒级），
// 否则返回估算值并触发后台重建。
func ChatLogsTotal(ctx context.Context) (int64, error) {
	wm := &chatLogsWM
	wm.mu.Lock()
	defer wm.mu.Unlock()

	if !wm.loaded {
		wm.loadLocked(ctx)
	}
	// 月份切换封存后，旧水位 max_id 落进封存区间 → 口径过期，重建
	if wm.valid && wm.maxID > 0 && wm.maxID <= int64(models.SealedMaxChatLogID()) {
		wm.valid = false
	}
	if wm.valid {
		return wm.incrementalTotalLocked(ctx)
	}
	if !wm.rebuilding {
		wm.rebuilding = true
		go rebuildChatLogsWatermark()
	}
	return estimateChatLogsRows(ctx)
}

// InvalidateChatLogsWatermark 清理日志后调用：删的都是老行，基数会虚高，
// 必须失效重算。同时删除持久化记录，防止失效后重启恢复出脏口径。
func InvalidateChatLogsWatermark(ctx context.Context) {
	wm := &chatLogsWM
	wm.mu.Lock()
	wm.loaded = true
	wm.valid = false
	wm.mu.Unlock()

	if err := models.DB.WithContext(ctx).Unscoped().
		Where(&models.Config{Key: models.KeyChatLogsCountWatermark}).
		Delete(&models.Config{}).Error; err != nil {
		slog.Error("delete chat_logs watermark config failed", "error", err)
	}
}

func (wm *chatLogsWatermark) loadLocked(ctx context.Context) {
	wm.loaded = true
	config, err := gorm.G[models.Config](models.DB).Where(&models.Config{Key: models.KeyChatLogsCountWatermark}).First(ctx)
	if err != nil {
		if err != gorm.ErrRecordNotFound {
			slog.Error("load chat_logs watermark failed", "error", err)
		}
		return
	}
	var v chatLogsWatermarkValue
	// 当前月口径：max_id==0 且 count==0（空月重建结果）同样有效；
	// max_id 已落进封存区间（跨月重启）则过期，走重建
	if json.Unmarshal([]byte(config.Value), &v) != nil || v.Count < 0 || (v.MaxID <= 0 && v.Count > 0) {
		return
	}
	if v.MaxID > 0 && v.MaxID <= int64(models.SealedMaxChatLogID()) {
		return
	}
	wm.maxID, wm.count, wm.valid = v.MaxID, v.Count, true
}

// incrementalTotalLocked 水位增量：COUNT 与 MAX 同一语句快照，
// 避免并发写入时水位推进越过尚未计入的行导致永久漏计。
// 只扫当前月分表 id > 水位的主键新增段；封存月行数来自注册表合计。
func (wm *chatLogsWatermark) incrementalTotalLocked(ctx context.Context) (int64, error) {
	var row chatLogsAggRow
	if err := models.DB.WithContext(ctx).Unscoped().Table(models.CurrentChatLogShardTable()).
		Select("COUNT(*) AS cnt, COALESCE(MAX(id), 0) AS max_id").
		Where("id > ?", wm.maxID).Scan(&row).Error; err != nil {
		return 0, err
	}
	wm.count += row.Cnt
	if row.MaxID > wm.maxID {
		wm.maxID = row.MaxID
	}
	return sealedRowsTotal() + wm.count, nil
}

// rebuildChatLogsWatermark 全量重建水位（后台执行，不持锁）。
// 水位只覆盖当前月分表；封存月合计在读取时由注册表给出。
// 统计走 Unscoped：chat_logs 只有硬删（log_cleanup / 手动清理），无软删记录。
func rebuildChatLogsWatermark() {
	var row chatLogsAggRow
	err := models.DB.Unscoped().Table(models.CurrentChatLogShardTable()).
		Select("COUNT(*) AS cnt, COALESCE(MAX(id), 0) AS max_id").Scan(&row).Error

	wm := &chatLogsWM
	wm.mu.Lock()
	if err != nil {
		wm.rebuilding = false
		wm.mu.Unlock()
		slog.Error("rebuild chat_logs watermark failed", "error", err)
		return
	}
	wm.maxID, wm.count, wm.valid, wm.rebuilding = row.MaxID, row.Cnt, true, false
	wm.mu.Unlock()

	saveChatLogsWatermark(context.Background(), row.MaxID, row.Cnt)
}

// estimateChatLogsRows 估算总数 = 封存月合计（注册表精确值）+ 当前月估算。
// 当前月用 information_schema 统计（毫秒级，实测误差 <1%）；非 MySQL（如
// SQLite）或统计缺失时退化为当前月精确 COUNT——表内只有当月增量，量级可控。
func estimateChatLogsRows(ctx context.Context) (int64, error) {
	current, err := estimateCurrentChatLogShardRows(ctx)
	if err != nil {
		return 0, err
	}
	return sealedRowsTotal() + current, nil
}

func estimateCurrentChatLogShardRows(ctx context.Context) (int64, error) {
	table := models.CurrentChatLogShardTable()
	var rows int64
	err := models.DB.WithContext(ctx).Raw(
		"SELECT TABLE_ROWS FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
		table).Scan(&rows).Error
	if err == nil && rows > 0 {
		return rows, nil
	}
	if err != nil {
		slog.Warn("estimate chat log shard rows failed, fallback to COUNT", "table", table, "error", err)
	}
	if err := models.DB.WithContext(ctx).Unscoped().Table(table).Count(&rows).Error; err != nil {
		return 0, err
	}
	return rows, nil
}

// sealedRowsTotal 封存月行数合计（注册表冻结值）。
func sealedRowsTotal() int64 {
	var total int64
	for _, s := range models.SealedChatLogShards() {
		total += s.RowsCount
	}
	return total
}

func saveChatLogsWatermark(ctx context.Context, maxID, count int64) {
	raw, err := json.Marshal(chatLogsWatermarkValue{MaxID: maxID, Count: count})
	if err != nil {
		return
	}
	config, err := gorm.G[models.Config](models.DB).Where(&models.Config{Key: models.KeyChatLogsCountWatermark}).First(ctx)
	if err == gorm.ErrRecordNotFound {
		if cerr := gorm.G[models.Config](models.DB).Create(ctx, &models.Config{
			Key:   models.KeyChatLogsCountWatermark,
			Value: string(raw),
		}); cerr != nil {
			slog.Error("save chat_logs watermark failed", "error", cerr)
		}
		return
	}
	if err != nil {
		slog.Error("load chat_logs watermark for save failed", "error", err)
		return
	}
	config.Value = string(raw)
	if _, uerr := gorm.G[models.Config](models.DB).Where(&models.Config{Key: models.KeyChatLogsCountWatermark}).Updates(ctx, config); uerr != nil {
		slog.Error("save chat_logs watermark failed", "error", uerr)
	}
}
