package service

import (
	"bytes"
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

func TestBuildUpstreamBodyNormalizesBillingCchAnthropic(t *testing.T) {
	raw := []byte(`{"model":"GLM-5.2","stream":true,` +
		`"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.138.cca; cc_entrypoint=cli; cch=2382e;"},` +
		`{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	body, err := buildUpstreamBody(raw, nil)
	if err != nil {
		t.Fatalf("buildUpstreamBody() error = %v", err)
	}
	want := "x-anthropic-billing-header: cc_version=2.1.138.cca; cc_entrypoint=cli; cch=00000;"
	if got := gjson.GetBytes(body, "system.0.text").String(); got != want {
		t.Fatalf("system.0.text=%q, want %q", got, want)
	}
	if got := gjson.GetBytes(body, "system.1.text").String(); got != "You are Claude Code." {
		t.Fatalf("system.1.text changed unexpectedly: %q", got)
	}
	if !gjson.GetBytes(body, "system.1.cache_control.type").Exists() {
		t.Fatalf("system.1 cache_control lost")
	}
}

func TestBuildUpstreamBodyNormalizesBillingCchOpenAIString(t *testing.T) {
	raw := []byte(`{"model":"GLM-5.3",` +
		`"messages":[{"role":"system","content":"x-anthropic-billing-header: cc_version=2.1.145.864; cc_entrypoint=cli; cch=5c4f6;"},` +
		`{"role":"user","content":"hello"}]}`)
	body, err := buildUpstreamBody(raw, nil)
	if err != nil {
		t.Fatalf("buildUpstreamBody() error = %v", err)
	}
	want := "x-anthropic-billing-header: cc_version=2.1.145.864; cc_entrypoint=cli; cch=00000;"
	if got := gjson.GetBytes(body, "messages.0.content").String(); got != want {
		t.Fatalf("messages.0.content=%q, want %q", got, want)
	}
}

func TestBuildUpstreamBodyNormalizesBillingCchOpenAIArray(t *testing.T) {
	raw := []byte(`{"model":"GLM-5.3",` +
		`"messages":[{"role":"system","content":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.145.864; cc_entrypoint=cli; cch=09e68;"}]},` +
		`{"role":"user","content":"hello"}]}`)
	body, err := buildUpstreamBody(raw, nil)
	if err != nil {
		t.Fatalf("buildUpstreamBody() error = %v", err)
	}
	want := "x-anthropic-billing-header: cc_version=2.1.145.864; cc_entrypoint=cli; cch=00000;"
	if got := gjson.GetBytes(body, "messages.0.content.0.text").String(); got != want {
		t.Fatalf("messages.0.content.0.text=%q, want %q", got, want)
	}
}

func TestBuildUpstreamBodyBillingCchIdempotent(t *testing.T) {
	raw := []byte(`{"model":"GLM-5.2",` +
		`"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.138.cca; cc_entrypoint=cli; cch=00000;"}],` +
		`"messages":[{"role":"user","content":"hello"}]}`)
	body, err := buildUpstreamBody(raw, nil)
	if err != nil {
		t.Fatalf("buildUpstreamBody() error = %v", err)
	}
	if !bytes.Equal(raw, body) {
		t.Fatalf("already-normalized body changed: %s", string(body))
	}
}

func TestBuildUpstreamBodyWithoutBillingBlockUnchanged(t *testing.T) {
	raw := []byte(`{"model":"GLM-5.2",` +
		`"system":[{"type":"text","text":"You are Claude Code. cch=abc12 mentioned in passing.","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":"hello"}]}`)
	body, err := buildUpstreamBody(raw, nil)
	if err != nil {
		t.Fatalf("buildUpstreamBody() error = %v", err)
	}
	if !bytes.Equal(raw, body) {
		t.Fatalf("body without billing block should pass through unchanged, got: %s", string(body))
	}
}
