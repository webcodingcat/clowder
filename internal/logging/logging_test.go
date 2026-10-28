package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestJSONWritesOneObjectPerLine(t *testing.T) {
	var out bytes.Buffer
	New(&out, JSON, slog.LevelInfo).Info("joined", "host", "node1")

	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("not JSON: %v: %q", err, out.String())
	}
	if line["msg"] != "joined" || line["host"] != "node1" {
		t.Errorf("unexpected line: %v", line)
	}
}

func TestRecordsBelowTheLevelAreDropped(t *testing.T) {
	var out bytes.Buffer
	logger := New(&out, Text, slog.LevelWarn)
	logger.Info("hidden")
	logger.Warn("shown")

	if strings.Contains(out.String(), "hidden") || !strings.Contains(out.String(), "shown") {
		t.Errorf("unexpected output: %q", out.String())
	}
}
