package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samber/lo"
	"llmio/consts"
	"llmio/models"
)

// seedVisionUpstream 准备一个可打满全流程的上游：
// httptest 200 服务 + anthropic provider + vision 模型 + 带权重的图像关联。
func seedVisionUpstream(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(srv.Close)

	cfg := fmt.Sprintf(`{"base_url":%q,"api_key":"k","version":"2023-06-01"}`, srv.URL)
	p := models.Provider{Name: "p1", Type: consts.StyleAnthropic, Config: cfg}
	p.ID = 1
	if err := models.DB.Create(&p).Error; err != nil {
		t.Fatal(err)
	}

	m := models.Model{Name: "vision", DisplayOrder: 5, MaxRetry: 2, TimeOut: 30}
	if err := models.DB.Create(&m).Error; err != nil {
		t.Fatal(err)
	}

	mp := models.ModelWithProvider{
		ModelID:          m.ID,
		ProviderID:       1,
		ProviderModel:    "up",
		ToolCall:         lo.ToPtr(true),
		StructuredOutput: lo.ToPtr(true),
		Image:            lo.ToPtr(true),
		Status:           lo.ToPtr(true),
		Weight:           1,
		MaxInputTokens:   1000000,
	}
	if err := models.DB.Create(&mp).Error; err != nil {
		t.Fatal(err)
	}
}

// 成功转发的请求，返回的 ChatLog 要记录请求带图与图片总像素
func TestBalanceChatRecordsImageFlagAndPixels(t *testing.T) {
	setupRoutingDB(t)
	seedVisionUpstream(t)

	body := fmt.Sprintf(`{"model":"vision","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}`,
		pngB64(t, 100, 50))
	before, err := BeforerAnthropic([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	meta, _, err := ProvidersWithMetaBymodelsName(allowAllCtx(), consts.StyleAnthropic, *before, 0)
	if err != nil {
		t.Fatal(err)
	}

	res, log, err := BalanceChat(context.Background(), time.Now(), consts.StyleAnthropic, *before, *meta, models.ReqMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if !log.Image {
		t.Error("log.Image = false, want true")
	}
	if log.ImagePixels != 100*50 {
		t.Errorf("log.ImagePixels = %d, want %d", log.ImagePixels, 100*50)
	}
}

// 纯文本请求的 ChatLog 不应标记带图
func TestBalanceChatTextOnlyLogHasNoImage(t *testing.T) {
	setupRoutingDB(t)
	seedVisionUpstream(t)

	before, err := BeforerAnthropic([]byte(`{"model":"vision","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}

	meta, _, err := ProvidersWithMetaBymodelsName(allowAllCtx(), consts.StyleAnthropic, *before, 0)
	if err != nil {
		t.Fatal(err)
	}

	res, log, err := BalanceChat(context.Background(), time.Now(), consts.StyleAnthropic, *before, *meta, models.ReqMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if log.Image {
		t.Error("log.Image = true, want false")
	}
	if log.ImagePixels != 0 {
		t.Errorf("log.ImagePixels = %d, want 0", log.ImagePixels)
	}
}

// 像素超限被拒的请求，落库的错误日志也要记录带图与像素数
func TestPixelOverflowErrorLogRecordsImage(t *testing.T) {
	setupRoutingDB(t)
	seedProvider(t, 1, consts.StyleAnthropic)
	flash := seedModel(t, "flash", 5)
	seedPixelAssoc(t, flash.ID, 1, incidentPixelLimit)

	before := Before{Model: "flash", toolCall: true, structuredOutput: true, image: true,
		inputTokens: 82000, imagePixels: incidentTenImagePixels}
	_, _, err := ProvidersWithMetaBymodelsName(allowAllCtx(), consts.StyleAnthropic, before, 0)
	if err == nil {
		t.Fatal("err = nil, want PromptImagesTooLargeError")
	}

	var saved models.ChatLog
	if err := models.DB.Table(models.CurrentChatLogShardTable()).First(&saved).Error; err != nil {
		t.Fatal(err)
	}
	if !saved.Image {
		t.Error("saved.Image = false, want true")
	}
	if saved.ImagePixels != incidentTenImagePixels {
		t.Errorf("saved.ImagePixels = %d, want %d", saved.ImagePixels, incidentTenImagePixels)
	}
}

// 模型不存在的请求，落库的错误日志同样要记录带图信息
func TestModelNotFoundErrorLogRecordsImage(t *testing.T) {
	setupRoutingDB(t)

	before := Before{Model: "ghost", image: true, imagePixels: 4321}
	_, _, err := ProvidersWithMetaBymodelsName(allowAllCtx(), consts.StyleAnthropic, before, 0)
	if err == nil {
		t.Fatal("err = nil, want not found model")
	}

	var saved models.ChatLog
	if err := models.DB.Table(models.CurrentChatLogShardTable()).First(&saved).Error; err != nil {
		t.Fatal(err)
	}
	if !saved.Image {
		t.Error("saved.Image = false, want true")
	}
	if saved.ImagePixels != 4321 {
		t.Errorf("saved.ImagePixels = %d, want 4321", saved.ImagePixels)
	}
}
