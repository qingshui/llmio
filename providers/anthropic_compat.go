package providers

// anthropic_compat.go ports the request-body normalization logic from
// ~/anthropic-proxy/src/compat.go so that downstream providers which strictly
// follow the Anthropic schema (only user|assistant roles inside messages)
// can accept requests emitted by newer Claude Code clients that inject
// system/tool/developer roles directly into the messages array.
//
// normalizeRoles:
//   - system: content merged into the top-level "system" field, message dropped;
//   - tool:   converted into a role=user message wrapping a tool_result block;
//   - any other non user|assistant role: rewritten to "user";
//   - existing top-level "system" is preserved and prepended.
//
// Returns (newBody, changedCount). When changedCount == 0 the original body is
// returned untouched.

import "encoding/json"

func normalizeRoles(body []byte) ([]byte, int) {
	if len(body) == 0 {
		return body, 0
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return body, 0
	}
	msgs, ok := v["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body, 0
	}

	changed := 0
	out := make([]any, 0, len(msgs))
	var systemParts []any

	if sys, ok := v["system"]; ok {
		switch s := sys.(type) {
		case string:
			if s != "" {
				systemParts = append(systemParts, map[string]any{"type": "text", "text": s})
			}
		case []any:
			systemParts = append(systemParts, s...)
		}
	}

	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "user", "assistant":
			out = append(out, msg)
		case "system":
			systemParts = appendSystemContent(systemParts, msg["content"])
			changed++
		case "tool":
			toolUseID, _ := msg["tool_use_id"].(string)
			if toolUseID == "" {
				toolUseID, _ = msg["id"].(string)
			}
			tr := map[string]any{"type": "tool_result"}
			if toolUseID != "" {
				tr["tool_use_id"] = toolUseID
			}
			tr["content"] = msg["content"]
			out = append(out, map[string]any{
				"role":    "user",
				"content": []any{tr},
			})
			changed++
		default:
			msg["role"] = "user"
			out = append(out, msg)
			changed++
		}
	}

	if changed == 0 {
		return body, 0
	}

	v["messages"] = out
	if len(systemParts) > 0 {
		v["system"] = systemParts
	} else if _, had := v["system"]; had {
		delete(v, "system")
	}
	n, err := json.Marshal(v)
	if err != nil {
		return body, changed
	}
	return n, changed
}

// appendSystemContent appends one system message's content to the systemParts
// block list. Strings become text blocks; arrays are spread; anything else is
// JSON-encoded as a text block.
func appendSystemContent(parts []any, content any) []any {
	switch c := content.(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]any{"type": "text", "text": c})
		}
	case []any:
		parts = append(parts, c...)
	default:
		if c != nil {
			b, _ := json.Marshal(c)
			parts = append(parts, map[string]any{"type": "text", "text": string(b)})
		}
	}
	return parts
}
