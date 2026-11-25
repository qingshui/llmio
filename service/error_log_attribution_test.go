package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/samber/lo"
	"llmio/consts"
	"llmio/models"
)

// 错误路径的 ChatLog 必须保留 AuthKey 归属（AuthKeyID）。
// 超限/未知模型的请求被拒时若日志不带 AuthKeyID，
// 管理界面会把普通 key 的失败请求误显示为"无归属"（admin）调用。

func lastChatLog(t *testing.T) models.ChatLog {
	t.Helper()
	var log models.ChatLog
	// 分库后错误日志落在当前月分表 chat_logs_YYYYMM
	if err := models.DB.Table(models.CurrentChatLogShardTable()).Last(&log).Error; err != nil {
		t.Fatal(err)
	}
	return log
}

func TestErrorChatLogKeepsAuthKeyID(t *testing.T) {
	t.Run("prompt too long", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		m := seedModel(t, "m-small", 1)
		seedAssoc(t, m.ID, 1, 1000, false, false, false)

		before := Before{Model: "m-small", inputTokens: 5000}
		_, _, err := ProvidersWithMetaBymodelsName(context.Background(), consts.StyleAnthropic, before, 42)
		var tooLong *PromptTooLongError
		if !errors.As(err, &tooLong) {
			t.Fatalf("err = %v, want PromptTooLongError", err)
		}

		log := lastChatLog(t)
		if log.AuthKeyID != 42 {
			t.Errorf("ChatLog.AuthKeyID = %d, want 42 (caller's auth key)", log.AuthKeyID)
		}
		if log.Name != "m-small" {
			t.Errorf("ChatLog.Name = %q, want m-small", log.Name)
		}
	})

	t.Run("images too large", func(t *testing.T) {
		setupRoutingDB(t)
		seedProvider(t, 1, consts.StyleAnthropic)
		m := seedModel(t, "m-pixels", 1)
		mp := models.ModelWithProvider{
			ModelID:         m.ID,
			ProviderID:      1,
			ProviderModel:   "up",
			ToolCall:        lo.ToPtr(false),
			StructuredOutput: lo.ToPtr(false),
			Image:           lo.ToPtr(true),
			Status:          lo.ToPtr(true),
			MaxInputTokens:  1000,
			MaxImagePixels:  20_000_000,
		}
		if err := models.DB.Create(&mp).Error; err != nil {
			t.Fatal(err)
		}

		before := Before{Model: "m-pixels", image: true, imagePixels: 26_000_000, inputTokens: 100}
		_, _, err := ProvidersWithMetaBymodelsName(context.Background(), consts.StyleAnthropic, before, 42)
		var tooBig *PromptImagesTooLargeError
		if !errors.As(err, &tooBig) {
			t.Fatalf("err = %v, want PromptImagesTooLargeError", err)
		}

		log := lastChatLog(t)
		if log.AuthKeyID != 42 {
			t.Errorf("ChatLog.AuthKeyID = %d, want 42 (caller's auth key)", log.AuthKeyID)
		}
	})

	t.Run("model not found", func(t *testing.T) {
		setupRoutingDB(t)

		_, _, err := ProvidersWithMetaBymodelsName(context.Background(), consts.StyleAnthropic, Before{Model: "nope"}, 42)
		if err == nil || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("err = %v, want not-found-model error naming the model", err)
		}

		log := lastChatLog(t)
		if log.AuthKeyID != 42 {
			t.Errorf("ChatLog.AuthKeyID = %d, want 42 (caller's auth key)", log.AuthKeyID)
		}
		if !strings.Contains(log.Error, "nope") {
			t.Errorf("ChatLog.Error = %q, want it to name the missing model", log.Error)
		}
	})
}
