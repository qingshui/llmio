package service

import (
	"errors"
	"slices"
	"testing"

	"llmio/consts"
	"llmio/models"
)

// 超长请求跨模型回退的完整路由行为
func TestLengthFallbackRouting(t *testing.T) {
	t.Run("reroutes to larger-context model", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		seedProvider(t, 2, consts.StyleAnthropic)
		seedModel(t, "m-x", 10)
		big := seedModel(t, "m-big", 5)
		if err := models.DB.Model(&models.Model{}).Where("id = ?", big.ID).Update("time_out", 7).Error; err != nil {
			t.Fatal(err)
		}
		seedAssoc(t, 1, 1, 1000, true, true, false)
		seedAssoc(t, big.ID, 2, 0, true, true, false) // 0 = 不限长度

		before := Before{Model: "m-x", toolCall: true, structuredOutput: true, inputTokens: 5000}
		meta, effBefore, err := ProvidersWithMetaBymodelsName(allowAllCtx(), consts.StyleAnthropic, before, 0)
		if err != nil {
			t.Fatal(err)
		}
		if effBefore.Model != "m-big" {
			t.Errorf("effective model = %q, want m-big", effBefore.Model)
		}
		if meta.TimeOut != 7 {
			t.Errorf("meta.TimeOut = %d, want 7 (fallback model's config)", meta.TimeOut)
		}
		if !slices.Equal(assocProviderIDs(t, meta), []uint{2}) {
			t.Errorf("providers = %v, want [2]", assocProviderIDs(t, meta))
		}
	})
	t.Run("prefers tool-call capable candidate", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		seedModel(t, "m-x", 1)
		notools := seedModel(t, "big-notools", 100)
		tools := seedModel(t, "big-tools", 1)
		seedAssoc(t, 1, 1, 1000, true, true, false)
		seedAssoc(t, notools.ID, 1, 0, false, false, false)
		seedAssoc(t, tools.ID, 1, 0, true, false, false)

		before := Before{Model: "m-x", toolCall: true, inputTokens: 5000}
		_, effBefore, err := ProvidersWithMetaBymodelsName(allowAllCtx(), consts.StyleAnthropic, before, 0)
		if err != nil {
			t.Fatal(err)
		}
		if effBefore.Model != "big-tools" {
			t.Errorf("effective model = %q, want big-tools", effBefore.Model)
		}

		// 不带工具调用时按展示顺序
		before.toolCall = false
		_, effBefore, err = ProvidersWithMetaBymodelsName(allowAllCtx(), consts.StyleAnthropic, before, 0)
		if err != nil {
			t.Fatal(err)
		}
		if effBefore.Model != "big-notools" {
			t.Errorf("effective model = %q, want big-notools", effBefore.Model)
		}
	})
	t.Run("respects allow list", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		seedProvider(t, 2, consts.StyleAnthropic)
		seedModel(t, "m-x", 1)
		seedModel(t, "m-big", 100)
		seedAssoc(t, 1, 1, 1000, true, true, false)
		seedAssoc(t, 2, 2, 0, true, true, false)

		// m-big 不在允许列表里 → 不回退，返回长度超限
		before := Before{Model: "m-x", inputTokens: 5000}
		_, _, err := ProvidersWithMetaBymodelsName(allowListCtx("m-x"), consts.StyleAnthropic, before, 0)
		var tooLong *PromptTooLongError
		if !errors.As(err, &tooLong) {
			t.Fatalf("err = %v, want PromptTooLongError", err)
		}
		if tooLong.Estimated != 5000 || tooLong.Max != 1000 {
			t.Errorf("got estimated=%d max=%d, want 5000/1000", tooLong.Estimated, tooLong.Max)
		}
	})
	t.Run("returns length exceeded when nothing fits", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		seedProvider(t, 2, consts.StyleAnthropic)
		seedModel(t, "m-x", 1)
		seedModel(t, "m-z", 2)
		seedAssoc(t, 1, 1, 1000, true, true, false)
		seedAssoc(t, 2, 2, 2000, true, true, false)

		// 允许列表里最长的上限也只有 2000 → 报错，Max 取跨模型最大上限
		before := Before{Model: "m-x", inputTokens: 5000}
		_, _, err := ProvidersWithMetaBymodelsName(allowAllCtx(), consts.StyleAnthropic, before, 0)
		var tooLong *PromptTooLongError
		if !errors.As(err, &tooLong) {
			t.Fatalf("err = %v, want PromptTooLongError", err)
		}
		if tooLong.Estimated != 5000 || tooLong.Max != 2000 {
			t.Errorf("got estimated=%d max=%d, want 5000/2000 (max across allowed models)", tooLong.Estimated, tooLong.Max)
		}
	})
	t.Run("image-capable candidate wins over larger context", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		seedModel(t, "m-x", 1)
		seedModel(t, "vision-fit", 1)
		seedModel(t, "big-text", 100)
		seedAssoc(t, 1, 1, 100, true, true, true)
		seedAssoc(t, 2, 1, 0, true, true, true)  // 放得下且支持图像
		seedAssoc(t, 3, 1, 0, true, true, false) // 放得下但不支持图像

		before := Before{Model: "m-x", image: true, toolCall: true, inputTokens: 5000}
		_, effBefore, err := ProvidersWithMetaBymodelsName(imageAutoRouteCtx(true), consts.StyleAnthropic, before, 0)
		if err != nil {
			t.Fatal(err)
		}
		if effBefore.Model != "vision-fit" {
			t.Errorf("effective model = %q, want vision-fit (image fallback takes priority)", effBefore.Model)
		}
	})
}
