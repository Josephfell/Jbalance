package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in    string
		want  slog.Level
		valid bool
	}{
		{"", slog.LevelInfo, true},
		{"info", slog.LevelInfo, true},
		{"INFO", slog.LevelInfo, true},
		{"debug", slog.LevelDebug, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"  error  ", slog.LevelError, true},
		{"nonsense", slog.LevelInfo, false},
	}
	for _, c := range cases {
		got, ok := ParseLevel(c.in)
		if got != c.want || ok != c.valid {
			t.Errorf("ParseLevel(%q) = (%v,%v), want (%v,%v)", c.in, got, ok, c.want, c.valid)
		}
	}
}

func TestNewLoggerJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, Config{Level: "info", Format: "json"})
	logger.Info("hello", slog.String("k", "v"))

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("expected JSON output, got %q: %v", buf.String(), err)
	}
	if rec["msg"] != "hello" || rec["k"] != "v" {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestNewLoggerTextFormatIsDefault(t *testing.T) {
	var buf bytes.Buffer
	// Empty format must default to text (human-readable), not JSON.
	logger := NewLogger(&buf, Config{})
	logger.Info("hello")
	out := buf.String()
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("empty format should default to text, got JSON-looking output: %q", out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("expected message in output, got %q", out)
	}
}

func TestLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, Config{Level: "warn", Format: "text"})
	logger.Info("suppressed")
	logger.Warn("emitted")
	out := buf.String()
	if strings.Contains(out, "suppressed") {
		t.Errorf("info line should be filtered at warn level, got %q", out)
	}
	if !strings.Contains(out, "emitted") {
		t.Errorf("warn line should be present, got %q", out)
	}
}

func TestComponentTagsRecords(t *testing.T) {
	var buf bytes.Buffer
	base := NewLogger(&buf, Config{Format: "json"})
	logger := Component(base, "dataplane")
	logger.Info("up")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if rec["component"] != "dataplane" {
		t.Errorf("expected component=dataplane, got %v", rec["component"])
	}
}

func TestContextRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, Config{Format: "json"}).With(slog.String("request_id", "abc123"))
	ctx := WithContext(context.Background(), logger)

	got := FromContext(ctx)
	got.Info("served")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if rec["request_id"] != "abc123" {
		t.Errorf("expected request_id threaded through context, got %v", rec["request_id"])
	}
}

func TestFromContextDefaultsWhenAbsent(t *testing.T) {
	// A nil context and a bare context must both yield a usable (default)
	// logger. Use a nil-valued variable rather than an untyped nil literal
	// (staticcheck SA1012 forbids passing a literal nil Context).
	var nilCtx context.Context
	if FromContext(nilCtx) == nil {
		t.Error("FromContext(nil) returned nil")
	}
	if FromContext(context.Background()) == nil {
		t.Error("FromContext(empty) returned nil")
	}
}
