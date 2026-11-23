package handler

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"llmio/common"
	"llmio/models"
)

// chat_logs 按月分表后的跨表查询层（分表基础设施见 models/chat_log_shard.go）。
// 核心不变式：分表 id 区间互不相交且全局升序=时间升序，
// 因此「新→旧逐表、表内 id DESC」的拼接即全局 id DESC 序。

// logShardCutoff 解析 days 参数为窗口起点；all 返回零值（不限时间）。
func logShardCutoff(c *gin.Context) (cutoff time.Time, all bool) {
	all, n := parseLogDays(c)
	if all {
		return time.Time{}, true
	}
	return time.Now().AddDate(0, 0, -n), false
}

// applyLogShardFilters 构建单分表查询：字段筛选，边界分表附加时间谓词。
// 整月落在窗口内的分表（Boundary=false）不加 created_at 谓词——分表本身就是月区间。
func applyLogShardFilters(query *gorm.DB, c *gin.Context, ref models.ChatLogShardRef, cutoff time.Time, all bool) *gorm.DB {
	query = applyLogFieldFilters(query, c)
	if !all && ref.Boundary {
		query = query.Where("created_at >= ?", cutoff)
	}
	return query
}

// mergePaginateChatLogs 跨分表归并分页：新→旧逐表取「剩余 offset+need」行、
// 凑满即停，无 OFFSET 下推。深翻页时每表至多扫 offset+limit 行（page_size ≤ 100
// 封顶），不劣化于原单表 OFFSET 的代价。
func mergePaginateChatLogs(ctx context.Context, refs []models.ChatLogShardRef, c *gin.Context, cutoff time.Time, all bool, params common.PaginationParams) ([]models.ChatLog, error) {
	remainingOffset := (params.Page - 1) * params.PageSize
	need := params.PageSize
	logs := make([]models.ChatLog, 0, params.PageSize)
	for _, ref := range refs {
		if need == 0 {
			break
		}
		batch := make([]models.ChatLog, 0, remainingOffset+need)
		query := applyLogShardFilters(models.DB.WithContext(ctx).Model(&models.ChatLog{}).Table(ref.Table), c, ref, cutoff, all).
			Order("id DESC").
			Limit(remainingOffset + need)
		if err := query.Find(&batch).Error; err != nil {
			return nil, err
		}
		if len(batch) <= remainingOffset {
			remainingOffset -= len(batch)
			continue
		}
		tail := batch[remainingOffset:]
		if len(tail) > need {
			tail = tail[:need]
		}
		logs = append(logs, tail...)
		need -= len(tail)
		remainingOffset = 0
	}
	return logs, nil
}

// logShardWindowMinID 返回边界分表内时间窗口中最早一行的 id
//（走 created_at 索引，毫秒级），窗口内无行返回 0。
func logShardWindowMinID(db *gorm.DB, table string, cutoff time.Time) int64 {
	var ids []int64
	err := db.Unscoped().Table(table).
		Where("created_at >= ?", cutoff).
		Order("created_at ASC").Limit(1).Pluck("id", &ids).Error
	if err != nil || len(ids) == 0 {
		return 0
	}
	return ids[0]
}

// countShardQuery 构建单分表的 COUNT 查询（Unscoped，口径见 GetRequestLogs：
// chat_logs 只有硬删，无软删记录）。沿袭 MariaDB 5.5 的计划规避策略：
//   - minID > 0（边界表 + 含 provider_name/status/style 筛选）：用 id >= minID
//     代替 created_at 范围，纯索引范围扫描；
//   - 边界表 + 仅覆盖索引内筛选（MySQL）：强制 idx_chat_logs_range_stats 计数；
//   - 整月覆盖表：只有字段筛选，无时间谓词，不存在原单表的时间范围坏计划。
func countShardQuery(db *gorm.DB, c *gin.Context, ref models.ChatLogShardRef, cutoff time.Time, all bool, minID int64) *gorm.DB {
	query := db.Model(&models.ChatLog{}).Table(ref.Table)
	switch {
	case minID > 0:
		query = applyLogFieldFilters(query, c).Where("id >= ?", minID)
	case !all && ref.Boundary && !logCountUncoveredFilter(c) && db.Dialector.Name() == "mysql":
		query = applyLogFieldFilters(query, c).
			Where("created_at >= ?", cutoff).
			Table(ref.Table + " FORCE INDEX (idx_chat_logs_range_stats)")
	case !all && ref.Boundary:
		query = applyLogFieldFilters(query, c).Where("created_at >= ?", cutoff)
	default:
		query = applyLogFieldFilters(query, c)
	}
	return query.Unscoped()
}

// fanoutCountLogs 跨分表求和 COUNT。id 点查路由单表；trace/session 点查
// 与低基数筛选逐表计数后求和。
func fanoutCountLogs(ctx context.Context, db *gorm.DB, c *gin.Context, refs []models.ChatLogShardRef, cutoff time.Time, all bool) (int64, error) {
	if idQuery := c.Query("id"); idQuery != "" {
		id, err := strconv.ParseUint(idQuery, 10, 64)
		if err != nil {
			return 0, nil
		}
		table, err := models.LocateChatLogTable(ctx, uint(id))
		if err != nil {
			return 0, err
		}
		var total int64
		if err := applyLogFieldFilters(db.Model(&models.ChatLog{}).Table(table).Unscoped(), c).
			Count(&total).Error; err != nil {
			return 0, err
		}
		return total, nil
	}

	var total int64
	for _, ref := range refs {
		minID := int64(0)
		if !all && ref.Boundary && logCountUncoveredFilter(c) {
			minID = logShardWindowMinID(db, ref.Table, cutoff)
			if minID == 0 {
				continue // 窗口内该表无行
			}
		}
		var n int64
		if err := countShardQuery(db, c, ref, cutoff, all, minID).Count(&n).Error; err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// findChatLogByID 按 id 路由取单条日志。路由表 miss 时逐封存表探测兜底
//（跨月在途等竞态残余）；最终未找到返回 gorm.ErrRecordNotFound。
func findChatLogByID(ctx context.Context, id string) (models.ChatLog, error) {
	id64, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return models.ChatLog{}, gorm.ErrRecordNotFound
	}
	table, err := models.LocateChatLogTable(ctx, uint(id64))
	if err != nil {
		return models.ChatLog{}, err
	}
	log, err := gorm.G[models.ChatLog](models.DB).Table(table).Where("id = ?", id64).First(ctx)
	if err == nil {
		return log, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return models.ChatLog{}, err
	}
	probeTable, perr := models.ProbeChatLogTable(ctx, uint(id64))
	if perr != nil {
		return models.ChatLog{}, gorm.ErrRecordNotFound
	}
	return gorm.G[models.ChatLog](models.DB).Table(probeTable).Where("id = ?", id64).First(ctx)
}

// nthNewestChatLogID 跨分表取「第 N 新」的 id，供按条数清理时做保留水位。
// 分表按新→旧取够 N 条即停；总行数不足 N 返回 found=false（全部保留）。
func nthNewestChatLogID(ctx context.Context, n int) (uint, bool, error) {
	refs, err := models.ChatLogShardsForWindow(ctx, time.Time{})
	if err != nil {
		return 0, false, err
	}
	count := 0
	for _, ref := range refs {
		need := n - count
		var ids []uint
		if err := models.DB.WithContext(ctx).Unscoped().Table(ref.Table).
			Order("id DESC").Limit(need).Pluck("id", &ids).Error; err != nil {
			return 0, false, err
		}
		count += len(ids)
		if count >= n {
			return ids[len(ids)-1], true, nil
		}
	}
	return 0, false, nil
}
