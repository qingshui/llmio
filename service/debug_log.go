package service

// debug_log.go provides per-authkey DEBUG logging of the full upstream
// request and response. Toggle is the AuthKey.Debug flag, propagated via
// context (ContextKeyAuthKeyDebug) by middleware/auth.go.
//
// When enabled, each proxied chat call logs:
//   - [DEBUG REQ] method, url, full headers, full body (after normalizeRoles + model rewrite)
//   - [DEBUG RESP] status, full response headers, body (non-stream: full; stream: not captured)
//
// Output goes to the same slog handler as the rest of llmio (logs/llmio.log).

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"llmio/consts"
	"log/slog"
)

// debugEnabled reports whether the current request has DEBUG logging on.
func debugEnabled(ctx context.Context) bool {
	v, _ := ctx.Value(consts.ContextKeyAuthKeyDebug).(bool)
	return v
}

// debugLogRequest dumps the outgoing upstream request. body is the final
// request body (after all rewrites), so the log reflects exactly what is sent.
func debugLogRequest(ctx context.Context, method, url string, header http.Header, body []byte) {
	if !debugEnabled(ctx) {
		return
	}
	hdr := make(map[string]string, len(header))
	for k, vv := range header {
		if len(vv) == 0 {
			continue
		}
		hdr[k] = vv[0]
	}
	slog.Info("[DEBUG REQ]",
		"method", method,
		"url", url,
		"headers", hdr,
		"body", string(body),
	)
}

// debugLogResponse dumps the upstream response. For non-stream responses the
// full body is logged; for stream responses (text/event-stream) only the first
// 2KB are captured to bound log volume. The response body is left consumable
// by the caller (it is restored via io.NopCloser).
func debugLogResponse(ctx context.Context, resp *http.Response) {
	if !debugEnabled(ctx) || resp == nil {
		return
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		// Streaming: do not consume body. Log meta only; body stays consumable by the forwarder.
		logResponseMeta(ctx, resp, "(stream, body not captured to preserve forwarding)")
		return
	}
	if resp.Body == nil {
		logResponseMeta(ctx, resp, "")
		return
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("[DEBUG RESP] read body failed", "error", err, "status", resp.StatusCode)
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return
	}
	preview := string(bodyBytes)
	if len(bodyBytes) > 8192 {
		preview = string(bodyBytes[:8192]) + "...(truncated, total=" + itoa(len(bodyBytes)) + " bytes)"
	}
	logResponseMeta(ctx, resp, preview)

	// Restore body so downstream proxying still works.
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
}

// logResponseMeta emits the [DEBUG RESP] line with status/headers/body.
func logResponseMeta(ctx context.Context, resp *http.Response, body string) {
	hdr := make(map[string]string, len(resp.Header))
	for k, vv := range resp.Header {
		if len(vv) == 0 {
			continue
		}
		hdr[k] = vv[0]
	}
	slog.Info("[DEBUG RESP]",
		"status", resp.StatusCode,
		"headers", hdr,
		"body", body,
	)
}

// itoa avoids importing strconv just for one int->string conversion.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// debugLogResponseReader is a helper for callers that already drained the
// body into a byte slice (e.g. non-OK error path in service/chat.go).
func debugLogResponseBytes(ctx context.Context, status int, header http.Header, body []byte) {
	if !debugEnabled(ctx) {
		return
	}
	hdr := make(map[string]string, len(header))
	for k, vv := range header {
		if len(vv) == 0 {
			continue
		}
		hdr[k] = vv[0]
	}
	slog.Info("[DEBUG RESP]",
		"status", status,
		"headers", hdr,
		"body", string(body),
	)
}

// ---------------------------------------------------------------------------
// Streaming response chunk recording
//
// For stream responses (text/event-stream) the body cannot be read up-front
// without breaking SSE forwarding. Instead we tap into the stream that is
// already TeeReadered for RecordLog, and emit one [DEBUG STREAM] log line per
// SSE event (events are separated by a blank line "\n\n"). Each line carries:
//   logId      - request id (the ChatLog.ID), so all chunks of one request group together
//   chunk      - 1-based event index
//   elapsed_ms - milliseconds since the request started
//   bytes      - event payload size in bytes
//   event      - full event payload (no truncation)
//
// debugStreamRecorder wraps an io.Reader; callers Read from it as normal and
// each completed SSE event is logged. The final partial chunk (if any) is
// flushed on EOF.

// debugStreamRecorder logs SSE events as they flow through a TeeReader.
type debugStreamRecorder struct {
	r     io.Reader
	logId uint
	start time.Time
	buf   []byte
	chunk int
}

func newDebugStreamRecorder(r io.Reader, logId uint, start time.Time) *debugStreamRecorder {
	return &debugStreamRecorder{r: r, logId: logId, start: start}
}

func (d *debugStreamRecorder) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if n > 0 {
		d.buf = append(d.buf, p[:n]...)
		// SSE events are separated by a blank line. Emit each complete event.
		for {
			// look for "\n\n" event boundary
			idx := indexDoubleNL(d.buf)
			if idx < 0 {
				break
			}
			event := d.buf[:idx+2]
			d.buf = d.buf[idx+2:]
			d.chunk++
			d.emit(event)
		}
	}
	if err != nil {
		// flush trailing partial event (no terminating blank line)
		if len(d.buf) > 0 {
			d.chunk++
			d.emit(d.buf)
			d.buf = nil
		}
		debugStreamEnd(d.logId, d.chunk, d.start)
	}
	return n, err
}

func (d *debugStreamRecorder) emit(event []byte) {
	slog.Info("[DEBUG STREAM]",
		"logId", d.logId,
		"chunk", d.chunk,
		"elapsed_ms", time.Since(d.start).Milliseconds(),
		"bytes", len(event),
		"event", string(event),
	)
}

func debugStreamEnd(logId uint, chunks int, start time.Time) {
	slog.Info("[DEBUG STREAM END]",
		"logId", logId,
		"chunks", chunks,
		"total_ms", time.Since(start).Milliseconds(),
	)
}

// indexDoubleNL returns the index of the first "\n\n" in b, or -1 if absent.
func indexDoubleNL(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == '\n' && b[i+1] == '\n' {
			return i
		}
	}
	return -1
}
