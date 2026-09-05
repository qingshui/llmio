package handler

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"llmio/common"
	"llmio/models"
)

// 首页统计涉及 chat_logs 全表/大范围扫描，60s 内复用计算结果
const metricsCacheTTL = 60 * time.Second

type metricsCacheItem struct {
	value     any
	expiresAt time.Time
}

var metricsCache sync.Map

func cachedCompute[T any](key string, compute func() (T, error)) (T, error) {
	if v, ok := metricsCache.Load(key); ok {
		if item, ok := v.(metricsCacheItem); ok && time.Now().Before(item.expiresAt) {
			if val, ok := item.value.(T); ok {
				return val, nil
			}
		}
	}
	result, err := compute()
	if err != nil {
		return result, err
	}
	metricsCache.Store(key, metricsCacheItem{value: result, expiresAt: time.Now().Add(metricsCacheTTL)})
	return result, nil
}

type MetricsRes struct {
	Reqs   int64 `json:"reqs"`
	Tokens int64 `json:"tokens"`
}

func computeMetrics(ctx context.Context, start time.Time) (MetricsRes, error) {
	chain := gorm.G[models.ChatLog](models.DB).Where("created_at >= ?", start)

	reqs, err := chain.Count(ctx, "id")
	if err != nil {
		return MetricsRes{}, err
	}
	var tokens sql.NullInt64
	if err := chain.Select("sum(total_tokens) as tokens").Scan(ctx, &tokens); err != nil {
		return MetricsRes{}, err
	}
	return MetricsRes{Reqs: reqs, Tokens: tokens.Int64}, nil
}

func Metrics(c *gin.Context) {
	days, err := strconv.Atoi(c.Param("days"))
	if err != nil {
		common.BadRequest(c, "Invalid days parameter")
		return
	}

	now := time.Now()
	year, month, day := now.Date()
	start := time.Date(year, month, day, 0, 0, 0, 0, now.Location()).AddDate(0, 0, -days)

	res, err := cachedCompute("metrics:"+strconv.Itoa(days), func() (MetricsRes, error) {
		return computeMetrics(c.Request.Context(), start)
	})
	if err != nil {
		common.InternalServerError(c, "Failed to compute metrics: "+err.Error())
		return
	}
	common.Success(c, res)
}

// 统计时间范围：今天/本周(周一起)/本月(1号起)/近30天(滚动)/全部
const (
	RangeToday = "today"
	RangeWeek  = "week"
	RangeMonth = "month"
	Range30d   = "30d"
	RangeAll   = "all"
)

// 解析范围名，返回起点；all 返回 hasFilter=false
func rangeCutoff(name string) (time.Time, bool) {
	now := time.Now()
	year, month, day := now.Date()
	today := time.Date(year, month, day, 0, 0, 0, 0, now.Location())
	switch name {
	case RangeToday:
		return today, true
	case RangeWeek:
		offset := int(now.Weekday()) - int(time.Monday)
		if offset < 0 {
			offset += 7
		}
		return today.AddDate(0, 0, -offset), true
	case RangeMonth:
		return time.Date(year, month, 1, 0, 0, 0, 0, now.Location()), true
	case Range30d:
		return today.AddDate(0, 0, -30), true
	case RangeAll:
		return time.Time{}, false
	}
	return time.Time{}, false
}

// 从请求解析 range 参数（默认近30天），非法值返回 ok=false
func rangeFromRequest(c *gin.Context) (name string, cutoff time.Time, hasFilter bool, ok bool) {
	name = c.DefaultQuery("range", Range30d)
	cutoff, hasFilter = rangeCutoff(name)
	return name, cutoff, hasFilter, name == RangeAll || hasFilter
}

type Count struct {
	Model string `json:"model"`
	Calls int64  `json:"calls"`
}

func Counts(c *gin.Context) {
	rng, cutoff, hasFilter, ok := rangeFromRequest(c)
	if !ok {
		common.BadRequest(c, "Invalid range parameter")
		return
	}

	results, err := cachedCompute("counts:"+rng, func() ([]Count, error) {
		results := make([]Count, 0)
		// Unscoped：去掉 deleted_at IS NULL，让 GROUP BY name 走覆盖索引（口径包含软删除记录）
		q := models.DB.
			Unscoped().
			Model(&models.ChatLog{})
		if hasFilter {
			q = q.Where("created_at >= ?", cutoff)
		}
		if err := q.
			Select("name as model, COUNT(*) as calls").
			Group("name").
			Order("calls DESC").
			Scan(&results).Error; err != nil {
			return nil, err
		}

		const topN = 5
		if len(results) > topN {
			var othersCalls int64
			for _, item := range results[topN:] {
				othersCalls += item.Calls
			}
			results = append(results[:topN], Count{
				Model: "others",
				Calls: othersCalls,
			})
		}
		return results, nil
	})
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}
	common.Success(c, results)
}

type ProjectCount struct {
	Project string `json:"project"`
	Calls   int64  `json:"calls"`
	Tokens  int64  `json:"tokens"`
}

type ProjectMetrics struct {
	Calls  []ProjectCount `json:"calls"`
	Tokens []ProjectCount `json:"tokens"`
}

// 按排序条件取 TopN，剩余聚合为 "others"（次数与 token 量都累加）
func rankProjectTopN(list []ProjectCount, less func(a, b ProjectCount) bool) []ProjectCount {
	sorted := append([]ProjectCount(nil), list...)
	sort.Slice(sorted, func(i, j int) bool { return less(sorted[i], sorted[j]) })

	const topN = 5
	if len(sorted) <= topN {
		return sorted
	}
	var others ProjectCount
	for _, item := range sorted[topN:] {
		others.Calls += item.Calls
		others.Tokens += item.Tokens
	}
	others.Project = "others"
	return append(sorted[:topN], others)
}

func computeProjectCounts(cutoff time.Time, hasFilter bool) (ProjectMetrics, error) {
	type authKeyCount struct {
		AuthKeyID uint  `gorm:"column:auth_key_id"`
		Calls     int64 `gorm:"column:calls"`
		Tokens    int64 `gorm:"column:tokens"`
	}

	rows := make([]authKeyCount, 0)
	// Unscoped：去掉 deleted_at IS NULL，让 GROUP BY auth_key_id 走覆盖索引（口径包含软删除记录）
	q := models.DB.
		Unscoped().
		Model(&models.ChatLog{})
	if hasFilter {
		q = q.Where("created_at >= ?", cutoff)
	}
	if err := q.
		Select("auth_key_id, COUNT(*) as calls, SUM(total_tokens) as tokens").
		Group("auth_key_id").
		Scan(&rows).Error; err != nil {
		return ProjectMetrics{}, err
	}

	ids := make([]uint, 0)
	for _, row := range rows {
		if row.AuthKeyID == 0 {
			continue
		}
		ids = append(ids, row.AuthKeyID)
	}

	keys := make([]models.AuthKey, 0)
	if len(ids) > 0 {
		if err := models.DB.
			Model(&models.AuthKey{}).
			Where("id IN ?", ids).
			Find(&keys).Error; err != nil {
			return ProjectMetrics{}, err
		}
	}

	keyMap := make(map[uint]string, len(keys))
	for _, key := range keys {
		keyMap[key.ID] = strings.TrimSpace(key.Name)
	}

	projectAgg := make(map[string]ProjectCount)
	for _, row := range rows {
		project := "-"
		if row.AuthKeyID == 0 {
			project = "admin"
		} else if name, ok := keyMap[row.AuthKeyID]; ok && name != "" {
			project = name
		}
		item := projectAgg[project]
		item.Calls += row.Calls
		item.Tokens += row.Tokens
		projectAgg[project] = item
	}

	list := make([]ProjectCount, 0, len(projectAgg))
	for project, item := range projectAgg {
		item.Project = project
		list = append(list, item)
	}

	return ProjectMetrics{
		Calls:  rankProjectTopN(list, func(a, b ProjectCount) bool { return a.Calls > b.Calls }),
		Tokens: rankProjectTopN(list, func(a, b ProjectCount) bool { return a.Tokens > b.Tokens }),
	}, nil
}

func ProjectCounts(c *gin.Context) {
	rng, cutoff, hasFilter, ok := rangeFromRequest(c)
	if !ok {
		common.BadRequest(c, "Invalid range parameter")
		return
	}

	res, err := cachedCompute("projects:"+rng, func() (ProjectMetrics, error) {
		return computeProjectCounts(cutoff, hasFilter)
	})
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}
	common.Success(c, res)
}
