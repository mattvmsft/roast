package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestResponseRecorderSerializesConcurrentResponses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	recorder := NewResponseRecorder("20260920T120000Z-1234abcd")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := recorder.Append("verifier", "responses", "model", "application/json", []byte(`{"id":"response"}`)); err != nil {
				t.Errorf("Append: %v", err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(dir, "runs", recorder.runID, ProviderResponsesFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 8 {
		t.Fatalf("response lines = %d, want 8", len(lines))
	}
	for i, line := range lines {
		var entry struct {
			Sequence int             `json:"sequence"`
			Body     json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Sequence != i+1 || string(entry.Body) != `{"id":"response"}` {
			t.Fatalf("entry %d = %#v", i, entry)
		}
	}
}

func TestResponseRecorderPreservesNativeEventStream(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	recorder := NewResponseRecorder("20261016T120000Z-1234abcd")
	body := ": keepalive\r\n\r\nevent: message_start\r\ndata: {\"text\":\"hello 🌍 <world>\"}\r\n\r\n"
	if err := recorder.Append("finder", "anthropic", "model", "text/event-stream", []byte(body)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "runs", recorder.runID, ProviderResponsesFile))
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		ContentType string `json:"content_type"`
		Body        string `json:"body"`
	}
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.ContentType != "text/event-stream" || entry.Body != body {
		t.Fatalf("native SSE bytes changed: %+v", entry)
	}
	if err := recorder.Append("finder", "anthropic", "model", "text/event-stream", []byte{0xff}); err == nil {
		t.Fatal("invalid UTF-8 would be silently changed by JSON string encoding")
	}
}
