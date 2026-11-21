package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/png"
	"testing"

	"github.com/samber/lo"
	"llmio/consts"
	"llmio/models"
)

// pngB64 生成一张纯色 PNG 的 base64，尺寸由参数决定，用于测试图片像素估算。
func pngB64(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func seedPixelAssoc(t *testing.T, modelID, providerID uint, maxImagePixels int) {
	t.Helper()
	mp := models.ModelWithProvider{
		ModelID:          modelID,
		ProviderID:       providerID,
		ProviderModel:    "up",
		ToolCall:         lo.ToPtr(true),
		StructuredOutput: lo.ToPtr(true),
		Image:            lo.ToPtr(true),
		Status:           lo.ToPtr(true),
		MaxInputTokens:   1000000,
		MaxImagePixels:   maxImagePixels,
	}
	if err := models.DB.Create(&mp).Error; err != nil {
		t.Fatal(err)
	}
}

// 2026-11-21 线上事故实测数字：内层 GLM-5.3-Flash 部署 9 张截图(19,568,132px)能过、
// 10 张(21,815,008px)必挂，硬顶约 20MP。用这些数字做回归。
const (
	incidentNineImagePixels = 7*1999*1124 + 2*1600*1200           // 19,568,132
	incidentTenImagePixels  = incidentNineImagePixels + 1999*1124 // 21,815,008
	incidentPixelLimit      = 20_000_000
)

// Anthropic 风格：tool_result 嵌套的 base64 图片要按真实尺寸计入像素
func TestEstimateImagePixelsAnthropicToolResult(t *testing.T) {
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}]}`,
		pngB64(t, 1999, 1124))
	got := estimateImagePixels([]byte(body))
	if got != 1999*1124 {
		t.Errorf("estimateImagePixels = %d, want %d", got, 1999*1124)
	}
}

// OpenAI data URI 与 Gemini inlineData 两种形态同样要计入
func TestEstimateImagePixelsAcrossStyles(t *testing.T) {
	openai := fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`,
		pngB64(t, 100, 50))
	if got := estimateImagePixels([]byte(openai)); got != 100*50 {
		t.Errorf("openAI data-uri pixels = %d, want %d", got, 100*50)
	}

	gemini := fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":%q}}]}]}`,
		pngB64(t, 30, 40))
	if got := estimateImagePixels([]byte(gemini)); got != 30*40 {
		t.Errorf("gemini inlineData pixels = %d, want %d", got, 30*40)
	}
}

// 多张图求和；解不出的图片按 defaultImagePixels 兜底
func TestEstimateImagePixelsSumsAndFallsBack(t *testing.T) {
	garbage := base64.StdEncoding.EncodeToString([]byte("definitely not an image"))
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":%q}},
		{"type":"image","source":{"type":"base64","data":%q}}]}]}`,
		pngB64(t, 100, 50), garbage)
	got := estimateImagePixels([]byte(body))
	want := 100*50 + defaultImagePixels
	if got != want {
		t.Errorf("estimateImagePixels = %d, want %d", got, want)
	}
}

// BeforerAnthropic 要把 tool_result 嵌套图同时反映到 image 标记与 imagePixels
func TestBeforerAnthropicPopulatesImagePixels(t *testing.T) {
	body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}]}`,
		pngB64(t, 1999, 1124))
	before, err := BeforerAnthropic([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !before.image {
		t.Error("before.image = false, want true")
	}
	if before.imagePixels != 1999*1124 {
		t.Errorf("before.imagePixels = %d, want %d", before.imagePixels, 1999*1124)
	}
}

// 按 max_image_pixels 过滤关联：0 表示不限制；能力放宽到最后一档时像素上限依然生效
func TestFindModelWithProvidersByImagePixels(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	seedProvider(t, 2, consts.StyleAnthropic)
	seedPixelAssoc(t, 1, 1, incidentPixelLimit) // 20M 限制
	seedPixelAssoc(t, 1, 2, 0)                  // 不限制

	tests := []struct {
		name      string
		imageCall bool
		pixels    int
		want      []uint
	}{
		{"nine images fit both", true, incidentNineImagePixels, []uint{1, 2}},
		{"ten images only unlimited", true, incidentTenImagePixels, []uint{2}},
		{"pixel limit survives capability relaxation", false, incidentTenImagePixels, []uint{2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findModelWithProviders(context.Background(), 1, consts.StyleAnthropic,
				Before{toolCall: true, structuredOutput: true, image: tt.imageCall, imagePixels: tt.pixels})
			if err != nil {
				t.Fatal(err)
			}
			want := tt.want
			gotIDs := providerIDs(got)
			if len(gotIDs) != len(want) {
				t.Fatalf("providerIDs = %v, want %v", gotIDs, want)
			}
			for i := range want {
				if gotIDs[i] != want[i] {
					t.Fatalf("providerIDs = %v, want %v", gotIDs, want)
				}
			}
		})
	}
}

// 全部图像关联都放不下时，回退也找不到候选 → 干净的 PromptImagesTooLargeError 而非 502
func TestImagePixelsOverflowReturnsCleanError(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	flash := seedModel(t, "flash", 5)
	seedPixelAssoc(t, flash.ID, 1, incidentPixelLimit)

	before := Before{Model: "flash", toolCall: true, structuredOutput: true, image: true,
		inputTokens: 82000, imagePixels: incidentTenImagePixels}
	_, _, err := ProvidersWithMetaBymodelsName(allowAllCtx(), consts.StyleAnthropic, before, 0)
	var pixErr *PromptImagesTooLargeError
	if !errors.As(err, &pixErr) {
		t.Fatalf("err = %v, want PromptImagesTooLargeError", err)
	}
	if pixErr.Pixels != incidentTenImagePixels {
		t.Errorf("Pixels = %d, want %d", pixErr.Pixels, incidentTenImagePixels)
	}
	if pixErr.Max != incidentPixelLimit {
		t.Errorf("Max = %d, want %d", pixErr.Max, incidentPixelLimit)
	}
}

// 原模型图像关联放不下、开关打开且允许列表里有更大预算的图像模型时 → 回退到那个模型
func TestImagePixelsOverflowFallsBackToBiggerVisionModel(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	seedProvider(t, 2, consts.StyleAnthropic)
	flash := seedModel(t, "flash", 5)
	big := seedModel(t, "big-vision", 9)
	seedPixelAssoc(t, flash.ID, 1, incidentPixelLimit)
	seedPixelAssoc(t, big.ID, 2, 0)

	before := Before{Model: "flash", toolCall: true, structuredOutput: true, image: true,
		inputTokens: 82000, imagePixels: incidentTenImagePixels}
	_, effBefore, err := ProvidersWithMetaBymodelsName(imageAutoRouteCtx(true), consts.StyleAnthropic, before, 0)
	if err != nil {
		t.Fatalf("err = %v, want reroute to big-vision", err)
	}
	if effBefore.Model != "big-vision" {
		t.Errorf("effBefore.Model = %q, want big-vision", effBefore.Model)
	}
}
