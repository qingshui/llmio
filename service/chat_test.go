package service

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestBuildUpstreamBodyDeletesSessionID(t *testing.T) {
	raw := []byte(`{"model":"local-model","session_id":"owu-session","messages":[{"role":"user","content":"hi"}]}`)
	body, err := buildUpstreamBody(raw, map[string]any{
		"temperature": 0.2,
		"session_id":  "extra-session",
	})
	if err != nil {
		t.Fatalf("buildUpstreamBody() error = %v", err)
	}

	if gjson.GetBytes(body, "session_id").Exists() {
		t.Fatalf("session_id should not be sent upstream, body=%s", string(body))
	}
	if got := gjson.GetBytes(body, "model").String(); got != "local-model" {
		t.Fatalf("model=%q, want local-model", got)
	}
	if got := gjson.GetBytes(body, "temperature").Float(); got != 0.2 {
		t.Fatalf("temperature=%v, want 0.2", got)
	}
}
