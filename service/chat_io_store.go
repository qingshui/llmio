package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"llmio/models"
)

// ChatIO records are stored on disk as append-only per-day log files:
//
//	logs/chat_io/<YYYY-MM-DD>.log
//
// Each record is two lines:
//
//	{"log_id":123,"type":"input","length":1234,"created_at":"..."}
//	<raw content, may contain newlines>
//
// Because content can contain newlines, length (in bytes) is recorded in the
// header and the reader reads exactly that many bytes from the following line.
// Writes are append-only and guarded by a per-day mutex so concurrent appends
// within a single process are safe.

const chatIODir = "logs/chat_io"

type chatIORecordType string

const (
	chatIOInput  chatIORecordType = "input"
	chatIOOutput chatIORecordType = "output"
)

type chatIOHeader struct {
	LogID     uint             `json:"log_id"`
	Type      chatIORecordType `json:"type"`
	Length    int              `json:"length"`
	CreatedAt time.Time        `json:"created_at"`
}

// per-day mutex: only one writer per day file at a time within this process.
var (
	chatIOMuMu sync.Mutex
	chatIOMus  = map[string]*sync.Mutex{}
)

func chatIOMuFor(date string) *sync.Mutex {
	chatIOMuMu.Lock()
	defer chatIOMuMu.Unlock()
	mu, ok := chatIOMus[date]
	if !ok {
		mu = &sync.Mutex{}
		chatIOMus[date] = mu
	}
	return mu
}

func chatIOFilePath(t time.Time) string {
	return filepath.Join(chatIODir, t.Format("2006-01-02")+".log")
}

// appendChatIORecord appends a header line + raw content line for logID at ts.
// Content is written verbatim (no escaping) so large bodies are stored as-is.
// A trailing newline separates records.
func appendChatIORecord(logID uint, rtype chatIORecordType, content string, ts time.Time) error {
	date := ts.Format("2006-01-02")
	mu := chatIOMuFor(date)
	mu.Lock()
	defer mu.Unlock()

	if err := os.MkdirAll(chatIODir, 0o755); err != nil {
		return fmt.Errorf("mkdir chat_io: %w", err)
	}
	path := chatIOFilePath(ts)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open chat_io file: %w", err)
	}
	defer f.Close()

	header := chatIOHeader{
		LogID:     logID,
		Type:      rtype,
		Length:    len(content),
		CreatedAt: ts,
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("marshal header: %w", err)
	}

	// header line + "\n" + content + "\n"
	if _, err := f.Write(hb); err != nil {
		return err
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		return err
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return err
	}
	return nil
}

// AppendChatIOInput appends an "input" record for logID at ts.
func AppendChatIOInput(logID uint, content string, ts time.Time) error {
	return appendChatIORecord(logID, chatIOInput, content, ts)
}

// AppendChatIOOutput serializes the OutputUnion to JSON and appends an "output" record.
// JSON preserves the SSE chunk-array structure so readers can reconstruct OutputUnion.
func AppendChatIOOutput(logID uint, output models.OutputUnion, ts time.Time) error {
	body, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("marshal output: %w", err)
	}
	return appendChatIORecord(logID, chatIOOutput, string(body), ts)
}

// ReadChatIO returns the input and output content for logID, located via
// the given ChatLog creation time (which determines the day file).
// Returns ("", "", nil) if no record exists for logID.
func ReadChatIO(logID uint, ts time.Time) (input string, output string, err error) {
	path := chatIOFilePath(ts)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("open chat_io file: %w", err)
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for {
		headerLine, err := r.ReadString('\n')
		if err != nil {
			break
		}
		headerLine = strings.TrimRight(headerLine, "\n")
		if headerLine == "" {
			continue
		}
		var h chatIOHeader
		if jerr := json.Unmarshal([]byte(headerLine), &h); jerr != nil {
			// skip malformed header
			continue
		}
		// read exactly h.Length bytes of content
		buf := make([]byte, h.Length)
		_, rerr := io.ReadFull(r, buf)
		if rerr != nil {
			break
		}
		// consume the trailing newline after content (if present)
		if _, serr := r.ReadString('\n'); serr != nil {
			// ok if EOF
		}
		if h.LogID != logID {
			continue
		}
		switch h.Type {
		case chatIOInput:
			input = string(buf)
		case chatIOOutput:
			output = string(buf)
		}
	}
	return input, output, nil
}

// DeleteChatIOByDate removes the entire day file for the given date.
// This is only safe to call when ALL chat_logs on that date are being deleted;
// otherwise records for logs that should be retained would also be lost.
// Returns true if the file was removed, false if it did not exist.
func DeleteChatIOByDate(t time.Time) (bool, error) {
	path := chatIOFilePath(t)
	err := os.Remove(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("remove chat_io file: %w", err)
	}
	return true, nil
}

// DeleteChatIOBeforeDate removes day files for all dates strictly before the
// given cutoff date. Files whose date is on or after the cutoff are kept.
// This is the safe variant of DeleteChatIOByDate for retention-based cleanup:
// old day files are removed wholesale, recent ones are untouched.
func DeleteChatIOBeforeDate(cutoff time.Time) (int, error) {
	removed := 0
	entries, err := os.ReadDir(chatIODir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read chat_io dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		name = strings.TrimSuffix(name, ".log")
		// filename is YYYY-MM-DD
		fileDate, perr := time.Parse("2006-01-02", name)
		if perr != nil {
			continue
		}
		// compare at day granularity: keep same day, drop strictly before
		if fileDate.Before(time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, cutoff.Location())) {
			if rerr := os.Remove(filepath.Join(chatIODir, e.Name())); rerr != nil {
				if errors.Is(rerr, os.ErrNotExist) {
					continue
				}
				return removed, fmt.Errorf("remove %s: %w", e.Name(), rerr)
			}
			removed++
		}
	}
	return removed, nil
}
