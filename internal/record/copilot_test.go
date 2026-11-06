package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResponseRecorderPreservesCopilotText(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	recorder := NewResponseRecorder("20261106T120000Z-1234abcd")
	body := "invalid JSON with a raw newline:\n{\"tool\":"
	if err := recorder.Append("finder", "copilot", "", "text/plain", []byte(body)); err != nil {
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
	if entry.ContentType != "text/plain" || entry.Body != body {
		t.Fatalf("CLI text changed: %+v", entry)
	}
	if err := recorder.Append("finder", "copilot", "", "text/plain", []byte{0xff}); err == nil {
		t.Fatal("invalid UTF-8 must not be silently changed")
	}
}
