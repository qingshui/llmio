package service

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"gorm.io/gorm"
	"llmio/balancers"
	"llmio/consts"
	"llmio/models"
	"llmio/pkg/token"
	"llmio/providers"
)

func BalanceChat(ctx context.Context, start time.Time, style string, before Before, providersWithMeta ProvidersWithMeta, reqMeta models.ReqMeta) (*http.Response, *models.ChatLog, error) {
	slog.Info("request", "model", before.Model, "stream", before.Stream, "tool_call", before.toolCall, "structured_output", before.structuredOutput, "image", before.image, "input_tokens", before.inputTokens)

	providerMap := providersWithMeta.ProviderMap

	// 收集重试过程中的err日志
	retryLog := make(chan models.ChatLog, providersWithMeta.MaxRetry)
	defer close(retryLog)

	go RecordRetryLog(context.Background(), retryLog)

	// 选择负载均衡策略
	var balancer balancers.Balancer
	// AuthKey 配了 Provider 优先级列表时，每次请求都走 Priority（按列表顺序优先，失败按序降级），
	// 不再叠加 sticky / breaker：sticky 会把降级后的备用 provider 粘住导致列表头部不再被尝试，
	// 且其缓存 key 不含 authkey 会跨 key 污染；breaker 在列表头部 provider 偶发失败时会把它排除。
	// 列表顺序是强约束，应每次重新选。
	providerPriorities, _ := ctx.Value(consts.ContextKeyAuthKeyProviderPriorities).([]uint)
	if len(providerPriorities) > 0 {
		balancer = balancers.NewPriority(providersWithMeta.WeightItems, providersWithMeta.PriorityItems)
	} else {
		switch providersWithMeta.Strategy {
		case consts.BalancerLottery:
			balancer = balancers.NewLottery(providersWithMeta.WeightItems)
		case consts.BalancerRotor:
			balancer = balancers.NewRotor(providersWithMeta.WeightItems)
		case consts.BalancerPriority:
			balancer = balancers.NewPriority(providersWithMeta.WeightItems, providersWithMeta.PriorityItems)
		default:
			balancer = balancers.NewLottery(providersWithMeta.WeightItems)
		}

		// 是否开启熔断
		if providersWithMeta.Breaker {
			balancer = balancers.BalancerWrapperBreaker(balancer)
		}

		// 是否开启 IP 亲和性（最外层，breaker 之外）
		if providersWithMeta.Sticky && reqMeta.RemoteIP != "" {
			ttl := time.Duration(providersWithMeta.StickyTTL) * time.Second
			balancer = balancers.NewSticky(balancer, before.Model, reqMeta.RemoteIP, ttl)
		}
	}

	// 设置请求超时
	responseHeaderTimeout := time.Second * time.Duration(providersWithMeta.TimeOut)
	// 流式超时时间缩短
	if before.Stream {
		responseHeaderTimeout = responseHeaderTimeout / 3
	}

	authKeyID, _ := ctx.Value(consts.ContextKeyAuthKeyID).(uint)
	authKeyIOLog, _ := ctx.Value(consts.ContextKeyAuthKeyIOLog).(bool)

	traceID, err := token.GenerateRandomChars(10)
	if err != nil {
		return nil, nil, err
	}

	timer := time.NewTimer(time.Second * time.Duration(providersWithMeta.TimeOut))
	defer timer.Stop()
	for retry := range providersWithMeta.MaxRetry {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-timer.C:
			return nil, nil, errors.New("retry time out")
		default:
			// 加权负载均衡
			id, err := balancer.Pop()
			if err != nil {
				return nil, nil, fmt.Errorf("balancer pop err: %v, traceID: %s", err, traceID)
			}

			modelWithProvider, ok := providersWithMeta.ModelWithProviderMap[id]
			if !ok {
				// 数据不一致，移除该模型避免下次重复命中
				balancer.Delete(id)
				continue
			}

			provider := providerMap[modelWithProvider.ProviderID]

			chatModel, err := providers.New(provider.Type, provider.Config, provider.Proxy)
			if err != nil {
				return nil, nil, err
			}

			client := providers.GetClient(responseHeaderTimeout, provider.Proxy)

			slog.Info("using provider", "provider", provider.Name, "model", modelWithProvider.ProviderModel)

			log := models.ChatLog{
				Name:           before.Model,
				TraceID:        traceID,
				ProviderModel:  modelWithProvider.ProviderModel,
				ProviderName:   provider.Name,
				Status:         consts.StatusRunning,
				Style:          style,
				UserAgent:      reqMeta.UserAgent,
				RemoteIP:       reqMeta.RemoteIP,
				AuthKeyID:      authKeyID,
				SessionID:      before.SessionID,
				ChatIO:         authKeyIOLog,
				Image:          before.image,
				ImagePixels:    before.imagePixels,
				Retry:          retry,
				ProxyTime:      time.Since(start),
				InputPrice:     lo.FromPtrOr(modelWithProvider.InputPrice, 0),
				CacheReadPrice: lo.FromPtrOr(modelWithProvider.CacheReadPrice, 0),
				OutputPrice:    lo.FromPtrOr(modelWithProvider.OutputPrice, 0),
				Currency:       modelWithProvider.Currency,
			}
			// 根据请求原始请求头 是否透传请求头 自定义请求头 构建新的请求头
			withHeader := lo.FromPtrOr(modelWithProvider.WithHeader, false)
			headers := BuildHeaders(reqMeta.Header, withHeader, modelWithProvider.CustomerHeaders, before.Stream)

			rawBody, err := buildUpstreamBody(before.raw, modelWithProvider.ExtraBody)
			if err != nil {
				retryLog <- log.WithError(err)
				balancer.Delete(id)
				continue
			}

			req, err := chatModel.BuildReq(ctx, headers, modelWithProvider.ProviderModel, rawBody)
			if err != nil {
				retryLog <- log.WithError(err)
				// 构建请求失败 移除待选
				balancer.Delete(id)
				continue
			}

			if dbgReqBody, rerr := io.ReadAll(req.Body); rerr == nil {
				debugLogRequest(ctx, req.Method, req.URL.String(), req.Header, dbgReqBody)
				req.Body = io.NopCloser(bytes.NewReader(dbgReqBody))
			} else {
				req.Body = io.NopCloser(bytes.NewReader(nil))
			}

			res, err := client.Do(req)
			if err != nil {
				retryLog <- log.WithError(err)
				// 请求失败 移除待选
				balancer.Delete(id)
				continue
			}

			if res.StatusCode != http.StatusOK {
				byteBody, err := io.ReadAll(res.Body)
				if err != nil {
					slog.Error("read body error", "error", err)
				}
				retryLog <- log.WithError(fmt.Errorf("status: %d, body: %s", res.StatusCode, string(byteBody)))
				debugLogResponseBytes(ctx, res.StatusCode, res.Header, byteBody)

				if res.StatusCode == http.StatusTooManyRequests {
					// 达到RPM限制 降低权重
					balancer.Reduce(id)
				} else {
					// 非RPM限制 移除待选
					balancer.Delete(id)
				}
				res.Body.Close()
				continue
			}

			if provider.ErrorMatcher != "" {
				contentType := strings.ToLower(res.Header.Get("Content-Type"))
				// 流式正常返回通常是 text/event-stream，不提前消费响应体避免影响转发。
				if !strings.Contains(contentType, "text/event-stream") {
					byteBody, err := io.ReadAll(res.Body)
					if err != nil {
						retryLog <- log.WithError(fmt.Errorf("read body failed: %w", err))
						balancer.Delete(id)
						res.Body.Close()
						continue
					}

					if matched, sample := matchProviderBodyError(string(byteBody), provider.ErrorMatcher); matched {
						retryLog <- log.WithError(fmt.Errorf("response matched provider error sample %q, body: %s", sample, string(byteBody)))
						balancer.Delete(id)
						res.Body.Close()
						continue
					}

					res.Body = io.NopCloser(bytes.NewReader(byteBody))
				}
			}

			debugLogResponse(ctx, res)

			balancer.Success(id)

			return res, &log, nil
		}
	}

	return nil, nil, fmt.Errorf("All retry failed, trace ID: %s", traceID)
}

var billingCchRe = regexp.MustCompile(`cch=[0-9a-fA-F]+`)

const billingBlockPrefix = "x-anthropic-billing-header:"

// normalizeBillingText 把 billing 头 block 里逐请求随机的 cch 归一为等长 0。
// 该 block 位于上下文最头部，随机值会让上游引擎的前缀缓存从 token0 失配，
// 导致整段多轮上下文每轮全量重算 prefill。
func normalizeBillingText(text string) (string, bool) {
	if !strings.HasPrefix(text, billingBlockPrefix) {
		return text, false
	}
	m := billingCchRe.FindString(text)
	if m == "" {
		return text, false
	}
	v := strings.TrimPrefix(m, "cch=")
	zeros := strings.Repeat("0", len(v))
	if v == zeros {
		return text, false
	}
	return strings.Replace(text, m, "cch="+zeros, 1), true
}

func normalizeBillingCch(rawBody []byte) []byte {
	setText := func(path, text string) {
		if nb, err := sjson.SetBytes(rawBody, path, text); err == nil {
			rawBody = nb
		} else {
			slog.Warn("normalize billing cch failed", "path", path, "error", err)
		}
	}
	// anthropic 风格：system 数组中的独立 billing block
	if sys := gjson.GetBytes(rawBody, "system"); sys.IsArray() {
		for i, blk := range sys.Array() {
			text := blk.Get("text")
			if !text.Exists() {
				continue
			}
			if nt, ok := normalizeBillingText(text.String()); ok {
				setText(fmt.Sprintf("system.%d.text", i), nt)
			}
		}
	}
	// openai 风格：头部 system/developer 消息（字符串或分块数组内容）
	for i := 0; ; i++ {
		role := gjson.GetBytes(rawBody, fmt.Sprintf("messages.%d.role", i))
		if !role.Exists() || (role.String() != "system" && role.String() != "developer") {
			break
		}
		content := gjson.GetBytes(rawBody, fmt.Sprintf("messages.%d.content", i))
		if content.IsArray() {
			for j, blk := range content.Array() {
				text := blk.Get("text")
				if !text.Exists() {
					continue
				}
				if nt, ok := normalizeBillingText(text.String()); ok {
					setText(fmt.Sprintf("messages.%d.content.%d.text", i, j), nt)
				}
			}
		} else if content.Exists() {
			if nt, ok := normalizeBillingText(content.String()); ok {
				setText(fmt.Sprintf("messages.%d.content", i), nt)
			}
		}
	}
	return rawBody
}

func buildUpstreamBody(raw []byte, extraBody map[string]any) ([]byte, error) {
	rawBody := raw
	if len(extraBody) > 0 {
		for key, value := range extraBody {
			var err error
			rawBody, err = sjson.SetBytes(rawBody, key, value)
			if err != nil {
				slog.Warn("failed to set extra body key", "key", key, "error", err)
			}
		}
	}

	rawBody = normalizeBillingCch(rawBody)

	rawBody, err := sjson.DeleteBytes(rawBody, "session_id")
	if err != nil {
		return nil, fmt.Errorf("delete session_id from upstream body: %w", err)
	}
	return rawBody, nil
}

func RecordRetryLog(ctx context.Context, retryLog chan models.ChatLog) {
	for log := range retryLog {
		if _, err := SaveChatLog(ctx, log); err != nil {
			slog.Error("save chat log error", "error", err)
		}
	}
}

func RecordLog(ctx context.Context, reqStart time.Time, reader io.ReadCloser, processer Processer, logId uint, before Before, ioLog bool, debug bool) {
	recordFunc := func() error {
		defer reader.Close()
		if ioLog {
			if err := AppendChatIOInput(logId, string(before.raw), time.Now()); err != nil {
				return err
			}
		}
		var procReader io.Reader = reader
		if debug && before.Stream {
			procReader = newDebugStreamRecorder(reader, logId, reqStart)
		}
		log, output, err := processer(ctx, procReader, before.Stream, reqStart)
		if err != nil {
			return err
		}
		log.Status = consts.StatusSuccess
		if _, err := updateChatLogByID(ctx, logId, *log); err != nil {
			return err
		}
		if ioLog {
			if err := AppendChatIOOutput(logId, *output, time.Now()); err != nil {
				return err
			}
		}
		return nil
	}
	if err := recordFunc(); err != nil {
		if _, err := updateChatLogByID(ctx, logId, models.ChatLog{
			Status: consts.StatusError,
			Error:  err.Error(),
		}); err != nil {
			slog.Error("record log error", "error", err)
		}
	}
}

func SaveChatLog(ctx context.Context, log models.ChatLog) (uint, error) {
	err := models.WithChatLogShardForInsert(ctx, func(tx *gorm.DB, table string) error {
		return gorm.G[models.ChatLog](tx).Table(table).Create(ctx, &log)
	})
	if err != nil {
		return 0, err
	}
	return log.ID, nil
}

// updateChatLogByID 按 id 路由到所属分表更新。路由表上 miss（rowsAffected==0）
// 时逐封存表主键点查探测兜底，覆盖跨月在途响应的 Update 竞态；
// 所有分表都不含该 id（可能已被清理）时按 0 影响行返回。
func updateChatLogByID(ctx context.Context, id uint, updates models.ChatLog) (int64, error) {
	table, err := models.LocateChatLogTable(ctx, id)
	if err != nil {
		return 0, err
	}
	rows, err := gorm.G[models.ChatLog](models.DB).Table(table).Where("id = ?", id).Updates(ctx, updates)
	if err != nil {
		return 0, err
	}
	if rows > 0 {
		return int64(rows), nil
	}
	probeTable, perr := models.ProbeChatLogTable(ctx, id)
	if perr != nil || probeTable == table {
		return 0, nil
	}
	probedRows, err := gorm.G[models.ChatLog](models.DB).Table(probeTable).Where("id = ?", id).Updates(ctx, updates)
	return int64(probedRows), err
}

func BuildHeaders(source http.Header, withHeader bool, customHeaders map[string]string, stream bool) http.Header {
	header := http.Header{}
	if withHeader {
		header = source.Clone()
	}

	if stream {
		header.Set("X-Accel-Buffering", "no")
	}

	header.Del("Authorization")
	header.Del("X-Api-Key")
	header.Del("X-Goog-Api-Key")

	for key, value := range customHeaders {
		header.Set(key, value)
	}

	return header
}

type ProvidersWithMeta struct {
	ModelWithProviderMap map[uint]models.ModelWithProvider
	WeightItems          map[uint]int
	PriorityItems        map[uint]int
	ProviderMap          map[uint]models.Provider
	MaxRetry             int
	TimeOut              int
	Strategy             string
	Breaker              bool
	Sticky               bool
	StickyTTL            int
}

// styleEnabledAssociations 返回已启用且 provider 类型与请求协议一致的关联基础查询。
func styleEnabledAssociations(style string) gorm.ChainInterface[models.ModelWithProvider] {
	providerIDs := models.DB.Model(&models.Provider{}).Select("id").Where("type = ?", style)
	return gorm.G[models.ModelWithProvider](models.DB).
		Where("status = ?", true).
		Where("provider_id IN (?)", providerIDs)
}

// enabledAssociations 返回模型下启用且 provider 类型与请求协议一致的关联查询。
// 协议过滤放在能力过滤之前，避免筛出的关联全属于其他协议而导致无可用 provider。
func enabledAssociations(modelID uint, style string) gorm.ChainInterface[models.ModelWithProvider] {
	return styleEnabledAssociations(style).Where("model_id = ?", modelID)
}

// allowedModelAssociations 返回 AuthKey 允许的模型下、已启用且协议一致的关联查询。
// 上下文里没有 AuthKey 信息（非全量且无允许列表）时 ok=false。
func allowedModelAssociations(ctx context.Context, style string) (gorm.ChainInterface[models.ModelWithProvider], bool) {
	allowAll, _ := ctx.Value(consts.ContextKeyAllowAllModel).(bool)
	if allowAll {
		return styleEnabledAssociations(style), true
	}
	allowed, _ := ctx.Value(consts.ContextKeyAllowModels).([]string)
	if len(allowed) == 0 {
		return nil, false
	}
	modelIDs := models.DB.Model(&models.Model{}).Select("id").Where("name IN ?", allowed)
	return styleEnabledAssociations(style).Where("model_id IN (?)", modelIDs), true
}

// findFallbackModel 从 AuthKey 允许的模型中寻找候选回退模型。
// requireImage 为 true 时候选关联必须支持图像；候选关联必须已启用、协议一致、输入长度不超限。
// 请求带工具调用时优先选同时支持工具调用的候选，再按 DisplayOrder 降序、ID 升序取第一个。
func findFallbackModel(ctx context.Context, style string, before Before, excludeModelID uint, requireImage bool) (*models.Model, error) {
	base, ok := allowedModelAssociations(ctx, style)
	if !ok {
		return nil, nil
	}
	chain := base.Where("model_id <> ?", excludeModelID)
	if requireImage {
		chain = chain.Where("image = ?", true)
	}
	if before.inputTokens > 0 {
		chain = chain.Where("(max_input_tokens = 0 OR max_input_tokens >= ?)", before.inputTokens)
	}
	if before.imagePixels > 0 {
		chain = chain.Where("(max_image_pixels = 0 OR max_image_pixels >= ?)", before.imagePixels)
	}
	assocs, err := chain.Find(ctx)
	if err != nil {
		return nil, err
	}
	if len(assocs) == 0 {
		return nil, nil
	}

	// 请求带工具调用时，优先选同时支持工具调用的候选
	toolCapable := make(map[uint]bool)
	for _, mp := range assocs {
		if lo.FromPtrOr(mp.ToolCall, false) {
			toolCapable[mp.ModelID] = true
		}
	}
	candidates, err := gorm.G[models.Model](models.DB).
		Where("id IN ?", lo.Uniq(lo.Map(assocs, func(mp models.ModelWithProvider, _ int) uint { return mp.ModelID }))).
		Find(ctx)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	slices.SortFunc(candidates, func(a, b models.Model) int {
		if before.toolCall && toolCapable[a.ID] != toolCapable[b.ID] {
			if toolCapable[a.ID] {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(b.DisplayOrder, a.DisplayOrder); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return &candidates[0], nil
}

// PromptTooLongError 表示估算的输入长度超过 AuthKey 允许范围内所有启用关联的上限（含回退候选模型），
// Max 取其中最大的 max_input_tokens，作为该用户实际可用的长度上限报给客户端。
type PromptTooLongError struct {
	Estimated int
	Max       int
}

func (e *PromptTooLongError) Error() string {
	return fmt.Sprintf("prompt is too long: about %d tokens > %d maximum", e.Estimated, e.Max)
}

// PromptImagesTooLargeError 表示请求图片总像素超过 AuthKey 允许范围内所有启用关联的上限（含回退候选模型），
// Max 取其中最大的 max_image_pixels，作为该用户实际可用的视觉预算报给客户端。
type PromptImagesTooLargeError struct {
	Pixels int
	Max    int
}

func (e *PromptImagesTooLargeError) Error() string {
	return fmt.Sprintf("request images too large: about %d pixels > %d maximum", e.Pixels, e.Max)
}

// findModelWithProviders 按能力和输入长度筛选关联。
// 能力过滤为空时依次放宽 structured_output、tool_call、image，避免因未声明能力而 500；长度上限始终生效。
func findModelWithProviders(ctx context.Context, modelID uint, style string, before Before) ([]models.ModelWithProvider, error) {
	type tier struct{ toolCall, structuredOutput, image bool }
	tiers := []tier{
		{before.toolCall, before.structuredOutput, before.image},
		{before.toolCall, false, before.image},
		{false, false, before.image},
		{false, false, false},
	}
	for i, t := range tiers {
		if i > 0 && t == tiers[i-1] {
			continue
		}
		chain := enabledAssociations(modelID, style)
		if before.inputTokens > 0 {
			chain = chain.Where("(max_input_tokens = 0 OR max_input_tokens >= ?)", before.inputTokens)
		}
		if before.imagePixels > 0 {
			chain = chain.Where("(max_image_pixels = 0 OR max_image_pixels >= ?)", before.imagePixels)
		}
		if t.toolCall {
			chain = chain.Where("tool_call = ?", true)
		}
		if t.structuredOutput {
			chain = chain.Where("structured_output = ?", true)
		}
		if t.image {
			chain = chain.Where("image = ?", true)
		}
		found, err := chain.Find(ctx)
		if err != nil {
			return nil, err
		}
		if len(found) > 0 {
			if i > 0 {
				slog.Warn("no provider matches all capability filters, relaxed",
					"model", before.Model, "input_tokens", before.inputTokens,
					"want_tool_call", before.toolCall, "want_structured_output", before.structuredOutput, "want_image", before.image,
					"used_tool_call", t.toolCall, "used_structured_output", t.structuredOutput, "used_image", t.image)
			}
			return found, nil
		}
	}
	return nil, nil
}

// hasImageCapableAssociation 判断模型名下是否存在已启用、协议一致、支持图像且输入长度不超限的关联。
func hasImageCapableAssociation(ctx context.Context, modelID uint, style string, before Before) (bool, error) {
	chain := enabledAssociations(modelID, style).Where("image = ?", true)
	if before.inputTokens > 0 {
		chain = chain.Where("(max_input_tokens = 0 OR max_input_tokens >= ?)", before.inputTokens)
	}
	if before.imagePixels > 0 {
		chain = chain.Where("(max_image_pixels = 0 OR max_image_pixels >= ?)", before.imagePixels)
	}
	count, err := chain.Count(ctx, "*")
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func ProvidersWithMetaBymodelsName(ctx context.Context, style string, before Before, authKeyID uint) (*ProvidersWithMeta, Before, error) {
	model, err := gorm.G[models.Model](models.DB).Where("name = ?", before.Model).First(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 错误日志必须带上 AuthKeyID 与有意义的错误信息，
			// 否则管理界面会把普通 key 的失败请求误显示为"无归属"（admin）调用。
			notFoundErr := errors.New("not found model " + before.Model)
			if _, err := SaveChatLog(ctx, models.ChatLog{
				Name:          before.Model,
				Status:        consts.StatusError,
				Style:         style,
				SessionID:     before.SessionID,
				Image:         before.image,
				ImagePixels:   before.imagePixels,
				AuthKeyID:     authKeyID,
				Error:         notFoundErr.Error(),
			}); err != nil {
				return nil, before, err
			}
			return nil, before, notFoundErr
		}
		return nil, before, err
	}

	// 带图请求且本模型没有能接住的图像关联时，
	// 从 AuthKey 允许的模型中改选支持图像的模型（找不到候选则维持原有放宽逻辑）。
	// 图片自动路由按项目（AuthKey.ImageAutoRoute）开关控制，默认关闭。
	imageAutoRoute, _ := ctx.Value(consts.ContextKeyImageAutoRoute).(bool)
	if before.image && imageAutoRoute {
		imageCapable, err := hasImageCapableAssociation(ctx, model.ID, style, before)
		if err != nil {
			return nil, before, err
		}
		if !imageCapable {
			fallback, err := findFallbackModel(ctx, style, before, model.ID, true)
			if err != nil {
				return nil, before, err
			}
			if fallback != nil {
				slog.Info("image request rerouted to image-capable model",
					"requested_model", before.Model, "fallback_model", fallback.Name)
				before.Model = fallback.Name
				model = *fallback
			}
		}
	}

	modelWithProviders, err := findModelWithProviders(ctx, model.ID, style, before)
	if err != nil {
		return nil, before, err
	}

	if len(modelWithProviders) == 0 {
		enabled, err := enabledAssociations(model.ID, style).Find(ctx)
		if err != nil {
			return nil, before, err
		}
		if len(enabled) == 0 {
			return nil, before, errors.New("not provider for model " + before.Model)
		}
		// 超长回退：X 的启用关联都放不下这个输入时，从允许列表里找放得下的模型。
		// 带图请求不走超长回退（图片自动路由由上面的开关单独控制），
		// 避免把带图请求送给无图像能力的模型。
		if !before.image {
			fallback, err := findFallbackModel(ctx, style, before, model.ID, false)
			if err != nil {
				return nil, before, err
			}
			if fallback != nil {
				slog.Info("long request rerouted to model with larger context",
					"requested_model", before.Model, "fallback_model", fallback.Name, "input_tokens", before.inputTokens)
				before.Model = fallback.Name
				model = *fallback
				modelWithProviders, err = findModelWithProviders(ctx, model.ID, style, before)
				if err != nil {
					return nil, before, err
				}
			}
		}
		// 允许列表里也没有放得下的模型 → 报超限错误，Max 取允许列表内所有启用关联的最大上限
		if len(modelWithProviders) == 0 {
			var maxLimit, maxPixels int
			for _, mp := range enabled {
				maxLimit = max(maxLimit, mp.MaxInputTokens)
				maxPixels = max(maxPixels, mp.MaxImagePixels)
			}
			if base, ok := allowedModelAssociations(ctx, style); ok {
				allowedAssocs, err := base.Find(ctx)
				if err != nil {
					return nil, before, err
				}
				for _, mp := range allowedAssocs {
					maxLimit = max(maxLimit, mp.MaxInputTokens)
					maxPixels = max(maxPixels, mp.MaxImagePixels)
				}
			}
			// 图片像素超限优先报更具体的错误，否则按输入长度超限报
			var overErr error
			if before.imagePixels > 0 && maxPixels > 0 && before.imagePixels > maxPixels {
				overErr = &PromptImagesTooLargeError{Pixels: before.imagePixels, Max: maxPixels}
			} else {
				overErr = &PromptTooLongError{Estimated: before.inputTokens, Max: maxLimit}
			}
			if _, err := SaveChatLog(ctx, models.ChatLog{
				Name:          before.Model,
				Status:        consts.StatusError,
				Style:         style,
				SessionID:     before.SessionID,
				Image:         before.image,
				ImagePixels:   before.imagePixels,
				AuthKeyID:     authKeyID,
				Error:         overErr.Error(),
			}); err != nil {
				return nil, before, err
			}
			return nil, before, overErr
		}
	}

	modelWithProviderMap := lo.KeyBy(modelWithProviders, func(mp models.ModelWithProvider) uint { return mp.ID })

	providers, err := gorm.G[models.Provider](models.DB).
		Where("id IN ?", lo.Map(modelWithProviders, func(mp models.ModelWithProvider, _ int) uint { return mp.ProviderID })).
		Where("type = ?", style).
		Find(ctx)
	if err != nil {
		return nil, before, err
	}

	providerMap := lo.KeyBy(providers, func(p models.Provider) uint { return p.ID })

	weightItems := make(map[uint]int)
	priorityItems := make(map[uint]int)
	providerPriorities, _ := ctx.Value(consts.ContextKeyAuthKeyProviderPriorities).([]uint)
	for _, mp := range modelWithProviders {
		if _, ok := providerMap[mp.ProviderID]; !ok {
			continue
		}
		weightItems[mp.ID] = mp.Weight
		priorityItems[mp.ID] = boostProviderPriority(mp.ProviderID, mp.Priority, providerPriorities)
	}

	return &ProvidersWithMeta{
		ModelWithProviderMap: modelWithProviderMap,
		WeightItems:          weightItems,
		PriorityItems:        priorityItems,
		ProviderMap:          providerMap,
		MaxRetry:             model.MaxRetry,
		TimeOut:              model.TimeOut,
		Strategy:             model.Strategy,
		Breaker:              lo.FromPtrOr(model.Breaker, false),
		Sticky:               lo.FromPtrOr(model.Sticky, false),
		StickyTTL:            model.StickyTTL,
	}, before, nil
}
