package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"gorm.io/gorm"
	"llmio/common"
	"llmio/consts"
	"llmio/models"
	"llmio/providers"
	"llmio/service"
)

// ProviderRequest represents the request body for creating/updating a provider
type ProviderRequest struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Config       string `json:"config"`
	Console      string `json:"console"`
	Proxy        string `json:"proxy"`
	ErrorMatcher string `json:"error_matcher"`
}

// ModelRequest represents the request body for creating/updating a model
type ModelRequest struct {
	Name      string `json:"name"`
	Remark    string `json:"remark"`
	MaxRetry  int    `json:"max_retry"`
	TimeOut   int    `json:"time_out"`
	Strategy  string `json:"strategy"`
	Breaker   bool   `json:"breaker"`
	Sticky    bool   `json:"sticky"`
	StickyTTL int    `json:"sticky_ttl"`
}

type ModelOrderRequest struct {
	ModelIDs []uint `json:"model_ids"`
}

// ModelWithProviderRequest represents the request body for creating/updating a model-provider association
type ModelWithProviderRequest struct {
	ModelID          uint              `json:"model_id"`
	ProviderModel    string            `json:"provider_name"`
	ProviderID       uint              `json:"provider_id"`
	ToolCall         bool              `json:"tool_call"`
	StructuredOutput bool              `json:"structured_output"`
	Image            bool              `json:"image"`
	WithHeader       bool              `json:"with_header"`
	CustomerHeaders  map[string]string `json:"customer_headers"`
	ExtraBody        map[string]any    `json:"extra_body"`
	Weight           int               `json:"weight"`
	Priority         int               `json:"priority"`
	MaxInputTokens   int               `json:"max_input_tokens"`
	InputPrice       float64           `json:"input_price"`
	CacheReadPrice   float64           `json:"cache_read_price"`
	OutputPrice      float64           `json:"output_price"`
	Currency         string            `json:"currency"`
}

// ModelProviderStatusRequest represents the request body for updating provider status
type ModelProviderStatusRequest struct {
	Status bool `json:"status"`
}

// SystemConfigRequest represents the request body for updating system configuration
type SystemConfigRequest struct {
	EnableSmartRouting  bool    `json:"enable_smart_routing"`
	SuccessRateWeight   float64 `json:"success_rate_weight"`
	ResponseTimeWeight  float64 `json:"response_time_weight"`
	DecayThresholdHours int     `json:"decay_threshold_hours"`
	MinWeight           int     `json:"min_weight"`
}

// ConfigValueRequest represents the request body for updating config value
type ConfigValueRequest struct {
	Value string `json:"value" binding:"required"`
}

// GetProviders 获取所有提供商列表（支持名称搜索和类型筛选）
func GetProviders(c *gin.Context) {
	// 筛选参数
	name := c.Query("name")
	providerType := c.Query("type")

	// 构建查询条件
	query := gorm.G[models.Provider](models.DB).Order("id DESC")

	if name != "" {
		query = query.Where("name LIKE ?", "%"+name+"%")
	}

	if providerType != "" {
		query = query.Where("type = ?", providerType)
	}

	providers, err := query.Find(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}

	common.Success(c, providers)
}

func GetProviderModels(c *gin.Context) {
	id := c.Param("id")
	provider, err := gorm.G[models.Provider](models.DB).Where("id = ?", id).First(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}
	chatModel, err := providers.New(provider.Type, provider.Config, provider.Proxy)
	if err != nil {
		common.InternalServerError(c, "Failed to get models: "+err.Error())
		return
	}
	models, err := chatModel.Models(c.Request.Context())
	if err != nil {
		common.NotFound(c, "Failed to get models: "+err.Error())
		return
	}
	common.Success(c, models)
}

// CreateProvider 创建提供商
func CreateProvider(c *gin.Context) {
	var req ProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}

	// Check if provider exists
	count, err := gorm.G[models.Provider](models.DB).Where("name = ?", req.Name).Count(c.Request.Context(), "id")
	if err != nil {
		common.InternalServerError(c, "Database error: "+err.Error())
		return
	}

	if count > 0 {
		common.BadRequest(c, "Provider already exists")
		return
	}

	provider := models.Provider{
		Name:         req.Name,
		Type:         req.Type,
		Config:       req.Config,
		Console:      req.Console,
		Proxy:        req.Proxy,
		ErrorMatcher: req.ErrorMatcher,
	}

	if err := gorm.G[models.Provider](models.DB).Create(c.Request.Context(), &provider); err != nil {
		common.InternalServerError(c, "Failed to create provider: "+err.Error())
		return
	}

	common.Success(c, provider)
}

// UpdateProvider 更新提供商
func UpdateProvider(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.BadRequest(c, "Invalid ID format")
		return
	}

	var req ProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}

	// Check if provider exists
	if _, err := gorm.G[models.Provider](models.DB).Where("id = ?", id).First(c.Request.Context()); err != nil {
		if err == gorm.ErrRecordNotFound {
			common.NotFound(c, "Provider not found")
			return
		}
		common.InternalServerError(c, "Database error: "+err.Error())
		return
	}

	// Update fields
	updates := models.Provider{
		Name:         req.Name,
		Type:         req.Type,
		Config:       req.Config,
		Console:      req.Console,
		Proxy:        req.Proxy,
		ErrorMatcher: req.ErrorMatcher,
	}

	if _, err := gorm.G[models.Provider](models.DB).Where("id = ?", id).Updates(c.Request.Context(), updates); err != nil {
		common.InternalServerError(c, "Failed to update provider: "+err.Error())
		return
	}

	// Get updated provider
	updatedProvider, err := gorm.G[models.Provider](models.DB).Where("id = ?", id).First(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, "Failed to retrieve updated provider: "+err.Error())
		return
	}

	common.Success(c, updatedProvider)
}

// DeleteProvider 删除提供商
func DeleteProvider(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.BadRequest(c, "Invalid ID format")
		return
	}

	result, err := gorm.G[models.Provider](models.DB).Where("id = ?", id).Delete(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, "Failed to delete provider: "+err.Error())
		return
	}

	//删除关联
	if _, err := gorm.G[models.ModelWithProvider](models.DB).Where("provider_id = ?", id).Delete(c.Request.Context()); err != nil {
		common.InternalServerError(c, "Failed to delete provider: "+err.Error())
		return
	}

	if result == 0 {
		common.NotFound(c, "Provider not found")
		return
	}

	common.Success(c, nil)
}

// GetModels 获取模型列表（支持分页与筛选）
func GetModels(c *gin.Context) {
	params, err := common.ParsePagination(c)
	if err != nil {
		common.BadRequest(c, err.Error())
		return
	}

	query := models.DB.Model(&models.Model{})

	if search := strings.TrimSpace(c.Query("search")); search != "" {
		like := "%" + search + "%"
		query = query.Where("name LIKE ?", like)
	}

	if strategy := strings.TrimSpace(c.Query("strategy")); strategy != "" {
		switch strategy {
		case consts.BalancerLottery, consts.BalancerRotor, consts.BalancerPriority:
			query = query.Where("strategy = ?", strategy)
		default:
			common.BadRequest(c, "invalid strategy filter")
			return
		}
	}

	list := make([]models.Model, 0)
	total, err := common.PaginateQuery(query.Order("id DESC"), params, &list)
	if err != nil {
		common.InternalServerError(c, "Failed to query models: "+err.Error())
		return
	}

	response := common.NewPaginationResponse(list, total, params)
	common.Success(c, response)
}

// GetModelList 返回所有模型列表用于下拉选择等场景
func GetModelList(c *gin.Context) {
	modelsList, err := gorm.G[models.Model](models.DB).Order("id DESC").Find(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, "Failed to get models: "+err.Error())
		return
	}

	common.Success(c, modelsList)
}

// CreateModel 创建模型
func CreateModel(c *gin.Context) {
	var req ModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}

	// Check if model exists
	count, err := gorm.G[models.Model](models.DB).Where("name = ?", req.Name).Count(c.Request.Context(), "id")
	if err != nil {
		common.InternalServerError(c, "Database error: "+err.Error())
		return
	}
	if count > 0 {
		common.BadRequest(c, fmt.Sprintf("Model: %s already exists", req.Name))
		return
	}
	strategy := req.Strategy
	if strategy == "" {
		strategy = consts.BalancerDefault
	}

	var maxDisplayOrder int
	if err := models.DB.Model(&models.Model{}).
		Select("COALESCE(MAX(display_order), 0)").
		Scan(&maxDisplayOrder).Error; err != nil {
		common.InternalServerError(c, "Failed to query model order: "+err.Error())
		return
	}

	model := models.Model{
		Name:         req.Name,
		Remark:       req.Remark,
		MaxRetry:     req.MaxRetry,
		TimeOut:      req.TimeOut,
		Strategy:     strategy,
		Breaker:      &req.Breaker,
		DisplayOrder: maxDisplayOrder + 1,
		Sticky:       &req.Sticky,
		StickyTTL:    req.StickyTTL,
	}

	if err := gorm.G[models.Model](models.DB).Create(c.Request.Context(), &model); err != nil {
		common.InternalServerError(c, "Failed to create model: "+err.Error())
		return
	}

	common.Success(c, model)
}

// UpdateModel 更新模型
func UpdateModel(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.BadRequest(c, "Invalid ID format")
		return
	}

	var req ModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}

	// Check if model exists
	_, err = gorm.G[models.Model](models.DB).Where("id = ?", id).First(c.Request.Context())
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			common.NotFound(c, "Model not found")
			return
		}
		common.InternalServerError(c, "Database error: "+err.Error())
		return
	}

	strategy := req.Strategy
	if strategy == "" {
		strategy = consts.BalancerDefault
	}

	// Update fields
	updates := models.Model{
		Name:      req.Name,
		Remark:    req.Remark,
		MaxRetry:  req.MaxRetry,
		TimeOut:   req.TimeOut,
		Strategy:  strategy,
		Breaker:   &req.Breaker,
		Sticky:    &req.Sticky,
		StickyTTL: req.StickyTTL,
	}

	if _, err := gorm.G[models.Model](models.DB).Where("id = ?", id).Updates(c.Request.Context(), updates); err != nil {
		common.InternalServerError(c, "Failed to update model: "+err.Error())
		return
	}

	// Force-write sticky_ttl so that 0 (use default) persists — GORM struct Updates skips zero-values.
	if err := models.DB.Model(&models.Model{}).Where("id = ?", id).Update("sticky_ttl", req.StickyTTL).Error; err != nil {
		common.InternalServerError(c, "Failed to update sticky_ttl: "+err.Error())
		return
	}

	// Get updated model
	updatedModel, err := gorm.G[models.Model](models.DB).Where("id = ?", id).First(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, "Failed to retrieve updated model: "+err.Error())
		return
	}

	common.Success(c, updatedModel)
}

// UpdateModelOrder 更新模型展示顺序
func UpdateModelOrder(c *gin.Context) {
	var req ModelOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}
	if len(req.ModelIDs) == 0 {
		common.BadRequest(c, "model_ids cannot be empty")
		return
	}

	seen := make(map[uint]struct{}, len(req.ModelIDs))
	modelIDs := make([]uint, 0, len(req.ModelIDs))
	for _, modelID := range req.ModelIDs {
		if modelID == 0 {
			common.BadRequest(c, "model_ids contains invalid value 0")
			return
		}
		if _, exists := seen[modelID]; exists {
			common.BadRequest(c, fmt.Sprintf("duplicated model_id: %d", modelID))
			return
		}
		seen[modelID] = struct{}{}
		modelIDs = append(modelIDs, modelID)
	}

	var existCount int64
	if err := models.DB.Model(&models.Model{}).Where("id IN ?", modelIDs).Count(&existCount).Error; err != nil {
		common.InternalServerError(c, "Failed to validate models: "+err.Error())
		return
	}
	if existCount != int64(len(modelIDs)) {
		common.BadRequest(c, "model_ids contains non-existing model")
		return
	}

	if err := models.DB.Transaction(func(tx *gorm.DB) error {
		total := len(modelIDs)
		for idx, modelID := range modelIDs {
			order := total - idx
			if err := tx.Model(&models.Model{}).
				Where("id = ?", modelID).
				Update("display_order", order).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		common.InternalServerError(c, "Failed to update model order: "+err.Error())
		return
	}

	slog.Info("UpdateModelOrder", "count", len(modelIDs))
	common.Success(c, map[string]any{"updated": len(modelIDs)})
}

// DeleteModel 删除模型
func DeleteModel(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.BadRequest(c, "Invalid ID format")
		return
	}

	result, err := gorm.G[models.Model](models.DB).Where("id = ?", id).Delete(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, "Failed to delete model: "+err.Error())
		return
	}

	if result == 0 {
		common.NotFound(c, "Model not found")
		return
	}

	common.Success(c, nil)
}

type ProviderTemplate struct {
	Type     string `json:"type"`
	Template string `json:"template"`
}

var template = []ProviderTemplate{
	{
		Type: "openai",
		Template: `{
			"base_url": "https://api.openai.com/v1",
			"api_key": "YOUR_API_KEY"
		}`,
	},
	{
		Type: "gemini",
		Template: `{
			"base_url": "https://generativelanguage.googleapis.com/v1beta",
			"api_key": "YOUR_GEMINI_API_KEY"
		}`,
	},
	{
		Type: "openai-res",
		Template: `{
			"base_url": "https://api.openai.com/v1",
			"api_key": "YOUR_API_KEY"
		}`,
	},
	{
		Type: "anthropic",
		Template: `{
			"base_url": "https://api.anthropic.com/v1",
			"api_key": "YOUR_API_KEY",
			"version": "2023-06-01"
		}`,
	},
}

func GetProviderTemplates(c *gin.Context) {
	common.Success(c, template)
}

// GetModelProviders 获取模型的提供商关联列表
func GetModelProviders(c *gin.Context) {
	modelIDStr := c.Query("model_id")
	if modelIDStr == "" {
		common.BadRequest(c, "model_id query parameter is required")
		return
	}

	modelID, err := strconv.ParseUint(modelIDStr, 10, 64)
	if err != nil {
		common.BadRequest(c, "Invalid model_id format")
		return
	}

	modelProviders, err := gorm.G[models.ModelWithProvider](models.DB).Where("model_id = ?", modelID).Find(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, err.Error())
		return
	}

	common.Success(c, modelProviders)
}

// GetModelProviderStatus 获取提供商状态信息
func GetModelProviderStatus(c *gin.Context) {
	providerIDStr := c.Query("provider_id")
	modelName := c.Query("model_name")
	providerModel := c.Query("provider_model")

	if providerIDStr == "" || modelName == "" || providerModel == "" {
		common.BadRequest(c, "provider_id, model_name and provider_model query parameters are required")
		return
	}

	providerID, err := strconv.ParseUint(providerIDStr, 10, 64)
	if err != nil {
		common.BadRequest(c, "Invalid provider_id format")
		return
	}

	// 获取提供商信息
	provider, err := gorm.G[models.Provider](models.DB).Where("id = ?", providerID).First(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, "Failed to retrieve provider: "+err.Error())
		return
	}

	// 获取最近10次请求状态
	logs, err := gorm.G[models.ChatLog](models.DB).
		Where("provider_name = ?", provider.Name).
		Where("provider_model = ?", providerModel).
		Where("name = ?", modelName).
		Where("status != ?", consts.StatusRunning).
		Limit(10).
		Order("created_at DESC").
		Find(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, "Failed to retrieve chat log: "+err.Error())
		return
	}

	status := make([]bool, 0)
	for _, log := range logs {
		status = append(status, log.Status == consts.StatusSuccess)
	}
	slices.Reverse(status)
	common.Success(c, status)
}

// CreateModelProvider 创建关联管理
func CreateModelProvider(c *gin.Context) {
	var req ModelWithProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}

	customerHeaders := req.CustomerHeaders
	if customerHeaders == nil {
		customerHeaders = map[string]string{}
	}

	extraBody := req.ExtraBody
	if extraBody == nil {
		extraBody = map[string]any{}
	}

	modelProvider := models.ModelWithProvider{
		ModelID:          req.ModelID,
		ProviderModel:    req.ProviderModel,
		ProviderID:       req.ProviderID,
		ToolCall:         &req.ToolCall,
		StructuredOutput: &req.StructuredOutput,
		Image:            &req.Image,
		WithHeader:       &req.WithHeader,
		CustomerHeaders:  customerHeaders,
		ExtraBody:        extraBody,
		Weight:           req.Weight,
		Priority:         req.Priority,
		MaxInputTokens:   req.MaxInputTokens,
		InputPrice:       &req.InputPrice,
		CacheReadPrice:   &req.CacheReadPrice,
		OutputPrice:      &req.OutputPrice,
		Currency:         req.Currency,
	}

	defaultStatus := true
	modelProvider.Status = &defaultStatus

	err := gorm.G[models.ModelWithProvider](models.DB).Create(c.Request.Context(), &modelProvider)
	if err != nil {
		common.InternalServerError(c, "Failed to create model-provider association: "+err.Error())
		return
	}

	common.Success(c, modelProvider)
}

// UpdateModelProvider 更新关联管理
func UpdateModelProvider(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.BadRequest(c, "Invalid ID format")
		return
	}

	var req ModelWithProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}
	slog.Info("UpdateModelProvider", "req", req)

	customerHeaders := req.CustomerHeaders
	if customerHeaders == nil {
		customerHeaders = map[string]string{}
	}

	extraBody := req.ExtraBody
	if extraBody == nil {
		extraBody = map[string]any{}
	}

	// Check if model-provider association exists
	_, err = gorm.G[models.ModelWithProvider](models.DB).Where("id = ?", id).First(c.Request.Context())
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			common.NotFound(c, "Model-provider association not found")
			return
		}
		common.InternalServerError(c, "Database error: "+err.Error())
		return
	}

	updates := models.ModelWithProvider{
		ModelID:          req.ModelID,
		ProviderID:       req.ProviderID,
		ProviderModel:    req.ProviderModel,
		ToolCall:         &req.ToolCall,
		StructuredOutput: &req.StructuredOutput,
		Image:            &req.Image,
		WithHeader:       &req.WithHeader,
		CustomerHeaders:  customerHeaders,
		ExtraBody:        extraBody,
		Weight:           req.Weight,
		Priority:         req.Priority,
		MaxInputTokens:   req.MaxInputTokens,
		InputPrice:       &req.InputPrice,
		CacheReadPrice:   &req.CacheReadPrice,
		OutputPrice:      &req.OutputPrice,
		Currency:         req.Currency,
	}

	if _, err := gorm.G[models.ModelWithProvider](models.DB).Where("id = ?", id).Updates(c.Request.Context(), updates); err != nil {
		common.InternalServerError(c, "Failed to update model-provider association: "+err.Error())
		return
	}

	// Force-write priority and max_input_tokens so that 0 (default/demote/unlimited) persists — GORM struct Updates skips zero-values.
	if err := models.DB.Model(&models.ModelWithProvider{}).Where("id = ?", id).Updates(map[string]any{
		"priority":         req.Priority,
		"max_input_tokens": req.MaxInputTokens,
	}).Error; err != nil {
		common.InternalServerError(c, "Failed to update priority and max_input_tokens: "+err.Error())
		return
	}

	// Get updated model-provider association
	updatedModelProvider, err := gorm.G[models.ModelWithProvider](models.DB).Where("id = ?", id).First(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, "Failed to retrieve updated model-provider association: "+err.Error())
		return
	}

	common.Success(c, updatedModelProvider)
}

// UpdateModelProviderStatus 切换关联管理启用状态
func UpdateModelProviderStatus(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.BadRequest(c, "Invalid ID format")
		return
	}

	var req ModelProviderStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request body: "+err.Error())
		return
	}

	existing, err := gorm.G[models.ModelWithProvider](models.DB).Where("id = ?", id).First(c.Request.Context())
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			common.NotFound(c, "Model-provider association not found")
			return
		}
		common.InternalServerError(c, "Failed to retrieve model-provider association: "+err.Error())
		return
	}

	status := req.Status
	updates := models.ModelWithProvider{
		Status: &status,
	}

	if _, err := gorm.G[models.ModelWithProvider](models.DB).Where("id = ?", id).Updates(c.Request.Context(), updates); err != nil {
		common.InternalServerError(c, "Failed to update status: "+err.Error())
		return
	}

	existing.Status = &status
	common.Success(c, existing)
}

// DeleteModelProvider 删除关联管理
func DeleteModelProvider(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil {
		common.BadRequest(c, "Invalid ID format")
		return
	}

	result, err := gorm.G[models.ModelWithProvider](models.DB).Where("id = ?", id).Delete(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, "Failed to delete model-provider association: "+err.Error())
		return
	}

	if result == 0 {
		common.NotFound(c, "Model-provider association not found")
		return
	}

	common.Success(c, nil)
}

type WrapLog struct {
	models.ChatLog
	KeyName string `json:"key_name"`
}

// chat_logs 的低基数筛选组合 COUNT 做短 TTL 缓存 + SWR：
// 过期后先返回旧值、后台单飞刷新，避免大 key（如 300 万行的 auth_key_id 筛选，
// COUNT 需数秒）在缓存过期后阻塞日志页。全表总数不走这里，改用水位线增量
// （service/chat_log_count.go）。trace_id/session_id/id 为高基数点查
// （本身毫秒级），不进缓存以免 key 无限膨胀。
const (
	logCountTTL     = 30 * time.Second
	logCountMaxSize = 256
)

type logCountEntry struct {
	total      int64
	expire     time.Time
	refreshing bool
}

var (
	logCountMu      sync.Mutex
	logCountEntries = map[string]logCountEntry{}
)

// logCountSWR：命中未过期直接返回；过期（含刷新中）返回旧值并后台单飞刷新；
// 未命中同步计算（筛选组合首次访问，通常几十 ms）。
func logCountSWR(key string, compute func() (int64, error)) (int64, error) {
	logCountMu.Lock()
	e, ok := logCountEntries[key]
	if ok {
		if time.Now().Before(e.expire) {
			logCountMu.Unlock()
			return e.total, nil
		}
		if !e.refreshing {
			e.refreshing = true
			logCountEntries[key] = e
			logCountMu.Unlock()
			go func() {
				t, err := compute()
				logCountMu.Lock()
				cur := logCountEntries[key]
				cur.refreshing = false
				if err == nil {
					cur.total, cur.expire = t, time.Now().Add(logCountTTL)
				}
				logCountEntries[key] = cur
				logCountMu.Unlock()
			}()
			return e.total, nil
		}
		logCountMu.Unlock()
		return e.total, nil
	}
	logCountMu.Unlock()

	t, err := compute()
	if err != nil {
		return 0, err
	}
	logCountCacheSet(key, t)
	return t, nil
}

func logCountCacheKey(providerName, name, status, style, authKeyID string) string {
	return providerName + "\x00" + name + "\x00" + status + "\x00" + style + "\x00" + authKeyID
}

func logCountCacheSet(key string, total int64) {
	logCountMu.Lock()
	if len(logCountEntries) >= logCountMaxSize {
		// 理论上只有低基数筛选会到这里，正常远达不到上限；防御性清空
		logCountEntries = map[string]logCountEntry{}
	}
	logCountEntries[key] = logCountEntry{total: total, expire: time.Now().Add(logCountTTL)}
	logCountMu.Unlock()
}

func logCountCacheReset() {
	logCountMu.Lock()
	logCountEntries = map[string]logCountEntry{}
	logCountMu.Unlock()
}

// applyLogFilters 按查询参数构建 chat_logs 筛选条件。
// COUNT 的 SWR 后台刷新会并发重建查询，不能与主查询共享 statement，故每次新建。
func applyLogFilters(query *gorm.DB, c *gin.Context) *gorm.DB {
	if v := c.Query("provider_name"); v != "" {
		query = query.Where("provider_name = ?", v)
	}
	if v := c.Query("name"); v != "" {
		query = query.Where("name = ?", v)
	}
	if v := c.Query("status"); v != "" {
		query = query.Where("status = ?", v)
	}
	if v := c.Query("style"); v != "" {
		query = query.Where("style = ?", v)
	}
	if v := c.Query("auth_key_id"); v != "" {
		query = query.Where("auth_key_id = ?", v)
	}
	if v := c.Query("trace_id"); v != "" {
		query = query.Where("trace_id = ?", v)
	}
	if v := c.Query("session_id"); v != "" {
		query = query.Where("session_id = ?", v)
	}
	if v := c.Query("id"); v != "" {
		query = query.Where("id = ?", v)
	}
	return query
}

// GetRequestLogs 获取最近的请求日志（支持分页和筛选）
func GetRequestLogs(c *gin.Context) {
	// 解析分页参数
	params, err := common.ParsePagination(c)
	if err != nil {
		common.BadRequest(c, err.Error())
		return
	}

	// 获取筛选参数
	providerName := c.Query("provider_name")
	name := c.Query("name")
	status := c.Query("status")
	style := c.Query("style")
	authKeyID := c.Query("auth_key_id")
	traceID := c.Query("trace_id")
	sessionID := c.Query("session_id")
	logID := c.Query("id")

	// with_count=false 时跳过 COUNT（total 返回 -1），供前端翻页复用已有总数
	withCount := c.Query("with_count") != "false"

	var total int64 = -1
	if withCount {
		// 统计总数走 Unscoped：去掉 deleted_at IS NULL 后才能纯用
		// idx_chat_logs_auth_key_id_tokens 覆盖索引计数，否则大 key 会回表扫描上百万行。
		// chat_logs 的删除均为硬删（log_cleanup / 手动清理），不存在软删除记录，口径不受影响。
		unfiltered := providerName == "" && name == "" && status == "" && style == "" && authKeyID == ""
		pointQuery := traceID != "" || sessionID != "" || logID != ""

		var countErr error
		switch {
		case unfiltered && !pointQuery:
			// 全表总数：水位线增量（精确，毫秒级），见 service/chat_log_count.go
			total, countErr = service.ChatLogsTotal(c.Request.Context())
		case pointQuery:
			// 点查命中行数少，直接 COUNT
			countErr = applyLogFilters(models.DB.Model(&models.ChatLog{}), c).Unscoped().Count(&total).Error
		default:
			// 低基数筛选组合：SWR 缓存，过期返回旧值 + 后台刷新
			total, countErr = logCountSWR(logCountCacheKey(providerName, name, status, style, authKeyID), func() (int64, error) {
				var t int64
				if err := applyLogFilters(models.DB.Model(&models.ChatLog{}), c).Unscoped().Count(&t).Error; err != nil {
					return 0, err
				}
				return t, nil
			})
		}
		if countErr != nil {
			common.InternalServerError(c, "Failed to count logs: "+countErr.Error())
			return
		}
	}

	var logs []models.ChatLog
	query := applyLogFilters(models.DB.Model(&models.ChatLog{}), c)
	if err := common.ApplyPagination(query.Order("id DESC"), params).Find(&logs).Error; err != nil {
		common.InternalServerError(c, "Failed to query logs: "+err.Error())
		return
	}

	keys, err := gorm.G[models.AuthKey](models.DB).Where("id IN ?", lo.Map(logs, func(log models.ChatLog, _ int) uint { return log.AuthKeyID })).Find(c.Request.Context())
	if err != nil {
		common.InternalServerError(c, "Failed to query auth keys: "+err.Error())
		return
	}

	keyMap := lo.KeyBy(keys, func(key models.AuthKey) uint { return key.ID })

	wrapLogs := make([]WrapLog, 0)
	for _, log := range logs {
		var keyName string
		if key, ok := keyMap[log.AuthKeyID]; ok {
			keyName = key.Name
		}
		if log.AuthKeyID == 0 {
			keyName = "admin"
		}
		wrapLogs = append(wrapLogs, WrapLog{
			ChatLog: log,
			KeyName: keyName,
		})
	}

	// 返回分页响应
	response := common.NewPaginationResponse(wrapLogs, total, params)
	common.Success(c, response)
}

// GetChatIO 查询指定日志的输入输出记录
func GetChatIO(c *gin.Context) {
	id := c.Param("id")

	// chat_io 内容已迁移到文件存储（logs/chat_io/<date>.log），
	// 数据库里的 chat_ios 表只保留与 chat_logs 的关联。
	// 这里通过 ChatLog 的 created_at 定位到对应日期的文件，再按 log_id 读取。
	chatLog, err := gorm.G[models.ChatLog](models.DB).Where("id = ?", id).First(c.Request.Context())
	if err != nil {
		common.NotFound(c, "ChatLog not found")
		return
	}

	input, outputBody, err := service.ReadChatIO(chatLog.ID, chatLog.CreatedAt)
	if err != nil {
		common.InternalServerError(c, "Failed to read chat IO: "+err.Error())
		return
	}

	// output 在写入时被序列化成 JSON，这里反序列化回 OutputUnion 以保持前端响应结构
	var output models.OutputUnion
	if outputBody != "" {
		if jerr := json.Unmarshal([]byte(outputBody), &output); jerr != nil {
			// 解析失败时退化为纯字符串展示
			output = models.OutputUnion{OfString: outputBody}
		}
	}

	common.Success(c, gin.H{
		"ID":            chatLog.ID,
		"CreatedAt":     chatLog.CreatedAt,
		"UpdatedAt":     chatLog.UpdatedAt,
		"LogId":         chatLog.ID,
		"Input":         input,
		"OfString":      output.OfString,
		"OfStringArray": output.OfStringArray,
		"Style":         chatLog.Style,
	})
}

// GetUserAgents 获取所有不重复的用户代理种类
func GetUserAgents(c *gin.Context) {
	var userAgents []string

	// 查询所有不重复的非空用户代理
	if err := models.DB.Model(&models.ChatLog{}).
		Where("user_agent IS NOT NULL AND user_agent != ''").
		Distinct("user_agent").
		Pluck("user_agent", &userAgents).
		Error; err != nil {
		common.InternalServerError(c, "Failed to query user agents: "+err.Error())
		return
	}

	common.Success(c, userAgents)
}

// GetConfigByKey 获取特定配置
func GetConfigByKey(c *gin.Context) {
	key := c.Param("key")
	config, err := gorm.G[models.Config](models.DB).Where(&models.Config{Key: key}).First(c.Request.Context())

	if err != nil {
		if err == gorm.ErrRecordNotFound {
			// 配置不存在，返回空响应
			common.Success(c, map[string]string{
				"key":   key,
				"value": "",
			})
			return
		}
		common.InternalServerError(c, "Failed to get config: "+err.Error())
		return
	}

	common.Success(c, map[string]string{
		"key":   config.Key,
		"value": config.Value,
	})
}

// UpdateConfigByKey 更新配置
func UpdateConfigByKey(c *gin.Context) {
	key := c.Param("key")

	var req ConfigValueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	// 获取或创建配置记录
	config, err := gorm.G[models.Config](models.DB).Where(&models.Config{Key: key}).First(c.Request.Context())
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			// 创建新配置
			config = models.Config{
				Key:   key,
				Value: req.Value,
			}
			if err := gorm.G[models.Config](models.DB).Create(c.Request.Context(), &config); err != nil {
				common.InternalServerError(c, "Failed to create config: "+err.Error())
				return
			}
		} else {
			common.InternalServerError(c, "Failed to get config: "+err.Error())
			return
		}
	} else {
		// 更新配置值
		config.Value = req.Value
		if _, err := gorm.G[models.Config](models.DB).Where(&models.Config{Key: key}).Updates(c.Request.Context(), config); err != nil {
			common.InternalServerError(c, "Failed to update config: "+err.Error())
			return
		}
	}

	common.Success(c, map[string]string{
		"key":   config.Key,
		"value": config.Value,
	})
}

// CleanLogsRequest 清理日志请求
type CleanLogsRequest struct {
	Type  string `json:"type"`  // "count" 或 "days"
	Value int    `json:"value"` // 数量或天数
}

// CleanLogs 清理日志
func CleanLogs(c *gin.Context) {
	start := time.Now()
	var req CleanLogsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if req.Value <= 0 {
		common.BadRequest(c, "Value must be greater than 0")
		return
	}

	var deletedCount int64

	switch req.Type {
	case "count":
		// 获取要保留的最小 ID
		var minID uint
		if err := models.DB.Model(&models.ChatLog{}).
			Order("id DESC").
			Limit(req.Value).
			Pluck("id", &[]uint{}).
			Scan(&minID).Error; err != nil {
			common.InternalServerError(c, "Failed to query min ID: "+err.Error())
			return
		}

		if minID == 0 {
			common.Success(c, map[string]any{"deleted_count": 0})
			return
		}

		// 先删除关联的 ChatIO
		if err := models.DB.Unscoped().Where("log_id IN (SELECT id FROM chat_logs WHERE id < ?)", minID).Delete(&models.ChatIO{}).Error; err != nil {
			common.InternalServerError(c, "Failed to delete chat IO: "+err.Error())
			return
		}

		// 删除日志
		result := models.DB.Unscoped().Where("id < ?", minID).Delete(&models.ChatLog{})
		if result.Error != nil {
			common.InternalServerError(c, "Failed to delete logs: "+result.Error.Error())
			return
		}
		deletedCount = result.RowsAffected

	case "days":
		count, err := service.CleanLogsByDays(c.Request.Context(), req.Value)
		if err != nil {
			common.InternalServerError(c, "Failed to delete logs: "+err.Error())
			return
		}
		deletedCount = count

	default:
		common.BadRequest(c, "Invalid type: must be 'count' or 'days'")
		return
	}

	// 日志已被删除，立即失效筛选计数缓存与全表总数水位线，避免日志页展示过期 total
	logCountCacheReset()
	service.InvalidateChatLogsWatermark(c.Request.Context())

	retentionDays := 0
	if req.Type == "days" {
		retentionDays = req.Value
	}
	record := models.LogCleanupRecord{
		RetentionDays: retentionDays,
		DeletedCount:  deletedCount,
		DurationMs:    time.Since(start).Milliseconds(),
		Source:        "manual",
		Type:          req.Type,
	}
	if err := gorm.G[models.LogCleanupRecord](models.DB).Create(c.Request.Context(), &record); err != nil {
		slog.Error("failed to save cleanup record", "error", err)
	}

	common.Success(c, map[string]any{"deleted_count": deletedCount})
}

// GetCleanupHistory 获取日志清理执行历史
func GetCleanupHistory(c *gin.Context) {
	params, err := common.ParsePagination(c)
	if err != nil {
		common.BadRequest(c, err.Error())
		return
	}

	query := models.DB.Model(&models.LogCleanupRecord{}).Order("id DESC")

	var records []models.LogCleanupRecord
	total, err := common.PaginateQuery(query, params, &records)
	if err != nil {
		common.InternalServerError(c, "Failed to query cleanup history: "+err.Error())
		return
	}

	common.Success(c, common.NewPaginationResponse(records, total, params))
}
