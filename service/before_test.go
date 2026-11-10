package service

import (
	"strings"
	"testing"
)

func TestBeforerAnthropicImageDetection(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"text only", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false},
		{"top-level image", `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"u"}}]}]}`, true},
		{"tool_result with nested image", `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"url","url":"u"}}]}]}]}`, true},
		{"tool_result with text only", `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"text","text":"ok"}]}]}]}`, false},
		{"tool_result with string content", `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}]}]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, err := BeforerAnthropic([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if before.image != tt.want {
				t.Errorf("image = %v, want %v", before.image, tt.want)
			}
		})
	}
}

func TestEstimateInputTokensExcludesBase64(t *testing.T) {
	blob := strings.Repeat("A", 300000)
	tests := []struct {
		name string
		body string
	}{
		{"anthropic source", `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + blob + `"}},{"type":"text","text":"hello"}]}]}`},
		{"openai data uri", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + blob + `"}},{"type":"text","text":"hello"}]}]}`},
		{"gemini inlineData", `{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + blob + `"}},{"text":"hello"}]}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateInputTokens([]byte(tt.body))
			if got > 1000 {
				t.Errorf("estimate = %d, base64 payload should not be counted", got)
			}
			if got == 0 {
				t.Errorf("estimate = 0, expected the non-base64 part to be counted")
			}
		})
	}
}

func TestEstimateInputTokensCountsText(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"` + strings.Repeat("a", 3000) + `"}]}`
	got := estimateInputTokens([]byte(body))
	if got < 1000 || got > 1100 {
		t.Errorf("estimate = %d, want about 1000", got)
	}
}
