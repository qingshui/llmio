package handler

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"llmio/common"
	"llmio/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
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

type Count struct {
	Model string `json:"model"`
	Calls int64  `json:"calls"`
}

func Counts(c *gin.Context) {
	results, err := cachedCompute("counts", func() ([]Count, error) {
		results := make([]Count, 0)
		// Unscoped：去掉 deleted_at IS NULL，让 GROUP BY name 走索引松散扫描（口径包含软删除记录）
		if err := models.DB.
			Unscoped().
			Model(&models.ChatLog{}).
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
}

func computeProjectCounts() ([]ProjectCount, error) {
	type authKeyCount struct {
		AuthKeyID uint  `gorm:"column:auth_key_id"`
		Calls     int64 `gorm:"column:calls"`
	}

	rows := make([]authKeyCount, 0)
	// Unscoped：去掉 deleted_at IS NULL，让 GROUP BY auth_key_id 走索引松散扫描（口径包含软删除记录）
	if err := models.DB.
		Unscoped().
		Model(&models.ChatLog{}).
		Select("auth_key_id, COUNT(*) as calls").
		Group("auth_key_id").
		Order("calls DESC").
		Scan(&rows).Error; err != nil {
		return nil, err
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
			return nil, err
		}
	}

	keyMap := make(map[uint]string, len(keys))
	for _, key := range keys {
		keyMap[key.ID] = strings.TrimSpace(key.Name)
	}

	projectCalls := make(map[string]int64)
	for _, row := range rows {
		project := "-"
		if row.AuthKeyID == 0 {
			project = "admin"
		} else if name, ok := keyMap[row.AuthKeyID]; ok && name != "" {
			project = name
		}
		projectCalls[project] += row.Calls
	}

	results := make([]ProjectCount, 0, len(projectCalls))
	for project, calls := range projectCalls {
		results = append(results, ProjectCount{
			Project: project,
			Calls:   calls,
		})
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Calls > results[j].Calls })

	const topN = 5
	if len(results) > topN {
		var othersCalls int64
		for _, item := range results[topN:] {
			othersCalls += item.Calls
		}
		results = append(results[:topN], ProjectCount{
			Project: "others",
			Calls:   othersCalls,
		})
	}
	return results, nil
}

func ProjectCounts(c *gin.Context) {
	results, err := cachedCompute("projects", func() ([]ProjectCount, error) {
		return computeProjectCounts()
	})
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}
	common.Success(c, results)
}
