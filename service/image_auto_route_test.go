package service

import (
	"context"
	"errors"
	"testing"

	"llmio/consts"
)

// imageAutoRouteCtx 返回开启了（或关闭了）图片自动路由开关的项目上下文。
func imageAutoRouteCtx(on bool) context.Context {
	return context.WithValue(allowAllCtx(), consts.ContextKeyImageAutoRoute, on)
}

// 开关关闭时：带图请求仍走原模型（能力放宽后命中自己的关联），不自动改选图像模型
func TestImageAutoRouteDisabledKeepsOriginalModel(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	seedProvider(t, 2, consts.StyleAnthropic)
	text := seedModel(t, "text", 10)
	vision := seedModel(t, "vision", 5)
	seedAssoc(t, text.ID, 1, 0, true, true, false)  // text 模型自己的无图像关联
	seedAssoc(t, vision.ID, 2, 0, true, true, true) // 允许列表里存在图像模型

	before := Before{Model: "text", toolCall: true, structuredOutput: true, image: true, inputTokens: 1000}
	_, effBefore, err := ProvidersWithMetaBymodelsName(imageAutoRouteCtx(false), consts.StyleAnthropic, before, 0)
	if err != nil {
		t.Fatalf("err = %v, want keep text model", err)
	}
	if effBefore.Model != "text" {
		t.Errorf("effBefore.Model = %q, want text (switch off must not reroute)", effBefore.Model)
	}
}

// 开关打开时：原模型无图像能力 → 自动改选允许列表里的图像模型
func TestImageAutoRouteEnabledReroutesToVisionModel(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	seedProvider(t, 2, consts.StyleAnthropic)
	text := seedModel(t, "text", 10)
	vision := seedModel(t, "vision", 5)
	seedAssoc(t, text.ID, 1, 0, true, true, false)
	seedAssoc(t, vision.ID, 2, 0, true, true, true)

	before := Before{Model: "text", toolCall: true, structuredOutput: true, image: true, inputTokens: 1000}
	_, effBefore, err := ProvidersWithMetaBymodelsName(imageAutoRouteCtx(true), consts.StyleAnthropic, before, 0)
	if err != nil {
		t.Fatalf("err = %v, want reroute to vision", err)
	}
	if effBefore.Model != "vision" {
		t.Errorf("effBefore.Model = %q, want vision", effBefore.Model)
	}
}

// 像素超限 + 存在更大视觉预算的图像模型：开关关闭 → 不回退，干净报错；
// 带图请求不走超长回退，避免被送去无图像能力的模型。
func TestImageAutoRouteDisabledPixelOverflowErrorsCleanly(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	seedProvider(t, 2, consts.StyleAnthropic)
	flash := seedModel(t, "flash", 5)
	big := seedModel(t, "big-vision", 9)
	seedPixelAssoc(t, flash.ID, 1, incidentPixelLimit)
	seedPixelAssoc(t, big.ID, 2, 0) // big-vision 完全放得下，但开关没开不允许自动改选

	before := Before{Model: "flash", toolCall: true, structuredOutput: true, image: true,
		inputTokens: 82000, imagePixels: incidentTenImagePixels}
	_, effBefore, err := ProvidersWithMetaBymodelsName(imageAutoRouteCtx(false), consts.StyleAnthropic, before, 0)
	var pixErr *PromptImagesTooLargeError
	if !errors.As(err, &pixErr) {
		t.Fatalf("err = %v, want PromptImagesTooLargeError", err)
	}
	if effBefore.Model != "flash" {
		t.Errorf("effBefore.Model = %q, want flash (switch off must not reroute)", effBefore.Model)
	}
}

// 纯文本超长回退不受图片开关影响：不带图的请求照常回退到大上下文模型
func TestLengthFallbackUnaffectedByImageSwitch(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	seedProvider(t, 2, consts.StyleAnthropic)
	small := seedModel(t, "small", 10)
	big := seedModel(t, "big", 9)
	seedAssoc(t, small.ID, 1, 500000, true, true, false)
	seedAssoc(t, big.ID, 2, 2000000, true, true, false)

	before := Before{Model: "small", toolCall: true, structuredOutput: true, inputTokens: 600000}
	_, effBefore, err := ProvidersWithMetaBymodelsName(imageAutoRouteCtx(false), consts.StyleAnthropic, before, 0)
	if err != nil {
		t.Fatalf("err = %v, want reroute to big", err)
	}
	if effBefore.Model != "big" {
		t.Errorf("effBefore.Model = %q, want big (text fallback must stay)", effBefore.Model)
	}
}
