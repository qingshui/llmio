package service

import (
	"context"
	"slices"
	"testing"

	"github.com/samber/lo"
	"llmio/consts"
	"llmio/models"
)

func seedModel(t *testing.T, name string, displayOrder int) models.Model {
	t.Helper()
	m := models.Model{Name: name, DisplayOrder: displayOrder}
	if err := models.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}
	return m
}

func seedDisabledImageAssoc(t *testing.T, modelID, providerID uint) {
	t.Helper()
	mp := models.ModelWithProvider{
		ModelID:       modelID,
		ProviderID:    providerID,
		ProviderModel: "up",
		Image:         lo.ToPtr(true),
		Status:        lo.ToPtr(false),
	}
	if err := models.DB.Create(&mp).Error; err != nil {
		t.Fatal(err)
	}
}

func allowAllCtx() context.Context {
	return context.WithValue(context.Background(), consts.ContextKeyAllowAllModel, true)
}

func allowListCtx(allowed ...string) context.Context {
	ctx := context.WithValue(context.Background(), consts.ContextKeyAllowAllModel, false)
	return context.WithValue(ctx, consts.ContextKeyAllowModels, allowed)
}

// 请求模型无图像关联时，从允许的模型中选出支持图像的候选
func TestFindImageFallbackModelSelectsImageModel(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	seedProvider(t, 2, consts.StyleAnthropic)
	text := seedModel(t, "text", 10)
	vision := seedModel(t, "vision", 5)
	seedAssoc(t, text.ID, 1, 0, true, true, false)
	seedAssoc(t, vision.ID, 2, 0, true, true, true)

	got, err := findFallbackModel(allowAllCtx(), consts.StyleAnthropic, Before{toolCall: true, image: true, inputTokens: 1000}, text.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "vision" {
		t.Errorf("fallback model = %v, want vision", got)
	}
}

// 请求带工具调用时优先选同时支持工具调用的候选；不带时按展示顺序
func TestFindImageFallbackModelPrefersToolCall(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	seedProvider(t, 2, consts.StyleAnthropic)
	text := seedModel(t, "text", 1)
	notools := seedModel(t, "vision-notools", 100)
	tools := seedModel(t, "vision-tools", 1)
	seedAssoc(t, text.ID, 1, 0, true, true, false)
	seedAssoc(t, notools.ID, 2, 0, false, false, true)
	seedAssoc(t, tools.ID, 1, 0, true, false, true)

	got, err := findFallbackModel(allowAllCtx(), consts.StyleAnthropic, Before{toolCall: true, image: true, inputTokens: 100}, text.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "vision-tools" {
		t.Errorf("tool-call fallback = %v, want vision-tools", got)
	}

	// 不带工具调用时按展示顺序，order 100 优先
	got, err = findFallbackModel(allowAllCtx(), consts.StyleAnthropic, Before{image: true, inputTokens: 100}, text.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "vision-notools" {
		t.Errorf("ordered fallback = %v, want vision-notools", got)
	}
}

// 多个候选按 DisplayOrder 降序、ID 升序取第一个
func TestFindImageFallbackModelOrdering(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	text := seedModel(t, "text", 1)
	low := seedModel(t, "vision-low", 5)
	high := seedModel(t, "vision-high", 9)
	seedAssoc(t, text.ID, 1, 0, true, true, false)
	seedAssoc(t, low.ID, 1, 0, true, true, true)
	seedAssoc(t, high.ID, 1, 0, true, true, true)

	got, err := findFallbackModel(allowAllCtx(), consts.StyleAnthropic, Before{image: true, inputTokens: 100}, text.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "vision-high" {
		t.Errorf("fallback = %v, want vision-high (higher display order)", got)
	}

	// 展示顺序相同 → ID 小者优先
	equal := seedModel(t, "vision-equal", 9)
	seedAssoc(t, equal.ID, 1, 0, true, true, true)
	got, err = findFallbackModel(allowAllCtx(), consts.StyleAnthropic, Before{image: true, inputTokens: 100}, text.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "vision-high" {
		t.Errorf("fallback = %v, want vision-high (smaller ID on tie)", got)
	}
}

// 仅从 AuthKey 允许的模型中挑选候选
func TestFindImageFallbackModelRespectsAllowList(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	text := seedModel(t, "text", 1)
	allowed := seedModel(t, "vision-allowed", 1)
	denied := seedModel(t, "vision-denied", 100)
	seedAssoc(t, text.ID, 1, 0, true, true, false)
	seedAssoc(t, allowed.ID, 1, 0, true, true, true)
	seedAssoc(t, denied.ID, 1, 0, true, true, true)

	got, err := findFallbackModel(allowListCtx("text", "vision-allowed"), consts.StyleAnthropic, Before{image: true, inputTokens: 100}, text.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != "vision-allowed" {
		t.Errorf("fallback = %v, want vision-allowed", got)
	}

	// 允许列表里没有图像模型 → 无候选
	got, err = findFallbackModel(allowListCtx("text"), consts.StyleAnthropic, Before{image: true, inputTokens: 100}, text.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("fallback = %v, want none", got)
	}
}

// 候选关联必须启用、协议一致、输入长度不超限
func TestFindImageFallbackModelFilters(t *testing.T) {
	t.Run("disabled association is not a candidate", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		text := seedModel(t, "text", 1)
		vision := seedModel(t, "vision", 2)
		seedAssoc(t, text.ID, 1, 0, true, true, false)
		seedDisabledImageAssoc(t, vision.ID, 1)
		got, err := findFallbackModel(allowAllCtx(), consts.StyleAnthropic, Before{image: true, inputTokens: 100}, text.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("fallback = %v, want none", got)
		}
	})
	t.Run("association of other style is not a candidate", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		seedProvider(t, 2, consts.StyleOpenAI)
		text := seedModel(t, "text", 1)
		vision := seedModel(t, "vision", 2)
		seedAssoc(t, text.ID, 1, 0, true, true, false)
		seedAssoc(t, vision.ID, 2, 0, true, true, true)
		got, err := findFallbackModel(allowAllCtx(), consts.StyleAnthropic, Before{image: true, inputTokens: 100}, text.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("fallback = %v, want none", got)
		}
	})
	t.Run("association over input limit is not a candidate", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		text := seedModel(t, "text", 1)
		vision := seedModel(t, "vision", 2)
		seedAssoc(t, text.ID, 1, 0, true, true, false)
		seedAssoc(t, vision.ID, 1, 500, true, true, true)
		got, err := findFallbackModel(allowAllCtx(), consts.StyleAnthropic, Before{image: true, inputTokens: 1000}, text.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("fallback = %v, want none", got)
		}
	})
	t.Run("zero limit association fits any input", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		text := seedModel(t, "text", 1)
		vision := seedModel(t, "vision", 2)
		seedAssoc(t, text.ID, 1, 0, true, true, false)
		seedAssoc(t, vision.ID, 1, 0, true, true, true)
		got, err := findFallbackModel(allowAllCtx(), consts.StyleAnthropic, Before{image: true, inputTokens: 1000000}, text.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil || got.Name != "vision" {
			t.Errorf("fallback = %v, want vision", got)
		}
	})
}

func assocProviderIDs(t *testing.T, meta *ProvidersWithMeta) []uint {
	t.Helper()
	ids := lo.Map(lo.Values(meta.ModelWithProviderMap), func(mp models.ModelWithProvider, _ int) uint { return mp.ProviderID })
	slices.Sort(ids)
	return ids
}

// 图像请求跨模型回退的完整路由行为
func TestImageFallbackRouting(t *testing.T) {
	t.Run("reroutes to image model and uses its config", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		seedProvider(t, 2, consts.StyleAnthropic)
		text := seedModel(t, "glm-4.6", 10)
		vision := seedModel(t, "glm-4.6v", 5)
		if err := models.DB.Model(&models.Model{}).Where("id = ?", vision.ID).Update("time_out", 42).Error; err != nil {
			t.Fatal(err)
		}
		seedAssoc(t, text.ID, 1, 0, true, true, false)
		seedAssoc(t, vision.ID, 2, 0, true, true, true)

		before := Before{Model: "glm-4.6", toolCall: true, structuredOutput: true, image: true, inputTokens: 1000}
		meta, effBefore, err := ProvidersWithMetaBymodelsName(imageAutoRouteCtx(true), consts.StyleAnthropic, before, 0)
		if err != nil {
			t.Fatal(err)
		}
		if effBefore.Model != "glm-4.6v" {
			t.Errorf("effective model = %q, want glm-4.6v", effBefore.Model)
		}
		if meta.TimeOut != 42 {
			t.Errorf("meta.TimeOut = %d, want 42 (fallback model's config)", meta.TimeOut)
		}
		if !slices.Equal(assocProviderIDs(t, meta), []uint{2}) {
			t.Errorf("providers = %v, want [2]", assocProviderIDs(t, meta))
		}
	})
	t.Run("keeps model that has image association", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		seedProvider(t, 2, consts.StyleAnthropic)
		seedModel(t, "text", 1)
		seedModel(t, "vision", 100)
		seedAssoc(t, 1, 1, 0, true, true, true)
		seedAssoc(t, 2, 2, 0, true, true, true)

		before := Before{Model: "text", toolCall: true, structuredOutput: true, image: true, inputTokens: 100}
		meta, effBefore, err := ProvidersWithMetaBymodelsName(allowAllCtx(), consts.StyleAnthropic, before, 0)
		if err != nil {
			t.Fatal(err)
		}
		if effBefore.Model != "text" {
			t.Errorf("effective model = %q, want text (no reroute)", effBefore.Model)
		}
		if !slices.Equal(assocProviderIDs(t, meta), []uint{1}) {
			t.Errorf("providers = %v, want [1]", assocProviderIDs(t, meta))
		}
	})
	t.Run("reroutes when own image association is too short", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		seedProvider(t, 2, consts.StyleAnthropic)
		seedModel(t, "text", 1)
		seedModel(t, "vision", 2)
		seedAssoc(t, 1, 1, 500, true, true, true)
		seedAssoc(t, 2, 2, 0, true, true, true)

		before := Before{Model: "text", image: true, inputTokens: 1000}
		_, effBefore, err := ProvidersWithMetaBymodelsName(imageAutoRouteCtx(true), consts.StyleAnthropic, before, 0)
		if err != nil {
			t.Fatal(err)
		}
		if effBefore.Model != "vision" {
			t.Errorf("effective model = %q, want vision", effBefore.Model)
		}
	})
	t.Run("no candidate keeps current behavior", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		seedModel(t, "text", 1)
		seedAssoc(t, 1, 1, 0, true, true, false)

		before := Before{Model: "text", toolCall: true, structuredOutput: true, image: true, inputTokens: 100}
		meta, effBefore, err := ProvidersWithMetaBymodelsName(allowAllCtx(), consts.StyleAnthropic, before, 0)
		if err != nil {
			t.Fatal(err)
		}
		if effBefore.Model != "text" {
			t.Errorf("effective model = %q, want text (no candidate)", effBefore.Model)
		}
		if !slices.Equal(assocProviderIDs(t, meta), []uint{1}) {
			t.Errorf("providers = %v, want [1] (relaxed within original model)", assocProviderIDs(t, meta))
		}
	})
}
