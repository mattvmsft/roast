package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func init() {
	if os.Getenv("ROAST_CLI_TEST_COPILOT_PROCESS") != "1" {
		return
	}
	input, err := io.ReadAll(os.Stdin)
	modelFlag := slices.Index(os.Args, "--model")
	if err != nil || !strings.Contains(string(input), "ROAST REQUEST:") ||
		modelFlag < 0 || modelFlag+1 >= len(os.Args) ||
		os.Args[modelFlag+1] != os.Getenv("ROAST_CLI_TEST_EXPECTED_MODEL") {
		fmt.Fprintln(os.Stderr, "routed CLI model override or prompt was lost")
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"type": "assistant.message",
		"data": map[string]any{
			"content": `{"tool":"findings","arguments":{"assessment":"NO_FINDINGS","findings":[]}}`,
		},
	})
	fmt.Println(`{"type":"assistant.turn_end","data":{}}`)
	os.Exit(0)
}

func TestCopilotAuthorRoutePreservesCLIOverrides(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		model     string
		wantUsage bool
	}{
		{
			name:  "copilot and shared model",
			args:  []string{"--copilot", "--model", "gpt-6.1-sol", "--finder-base-url", ""},
			model: "gpt-6.1-sol",
		},
		{
			name:  "finder model beats shared model",
			args:  []string{"--copilot", "--model", "auto", "--finder-model", "claude-selected", "--finder-base-url", ""},
			model: "claude-selected",
		},
		{
			name:  "role protocol retains routed model",
			args:  []string{"--finder-protocol", "copilot", "--finder-base-url", ""},
			model: "claude-routed",
		},
		{
			name:      "explicit API endpoint fails closed",
			args:      []string{"--copilot"},
			wantUsage: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("ROAST_RECORD_DIR", t.TempDir())
			var apiCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				apiCalls.Add(1)
				http.Error(w, "CLI selection must not send inference here", http.StatusInternalServerError)
			}))
			defer server.Close()
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			body := fmt.Sprintf(`version: 1
inference_profiles:
  primary:
    protocol: responses
    model: gpt-primary
  routed:
    protocol: anthropic
    endpoint: %s/v1
    api_key: fixture-key
    model: claude-routed
roles:
  finder:
    profile: primary
    author_routes:
      openai: routed
  verifier:
    profile: primary
`, server.URL)
			if err := os.WriteFile(configPath, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("ROAST_COPILOT_COMMAND", self)
			t.Setenv("ROAST_CLI_TEST_COPILOT_PROCESS", "1")
			t.Setenv("ROAST_CLI_TEST_EXPECTED_MODEL", test.model)
			t.Chdir(initChangedRepo(t))
			args := []string{
				"review", "--config", configPath, "--author-model", "gpt-6.1-sol",
				"--quick", "--format", "json", "--max-attempts", "1",
			}
			args = append(args, test.args...)
			var stdout, stderr bytes.Buffer
			code, runErr := ExecuteContext(t.Context(), args, &stdout, &stderr)
			if apiCalls.Load() != 0 {
				t.Fatalf("explicit CLI selection leaked %d request(s) to routed API: %v", apiCalls.Load(), runErr)
			}
			if test.wantUsage {
				if code != ExitUsage || runErr == nil || !strings.Contains(runErr.Error(), "copilot protocol does not use an endpoint") {
					t.Fatalf("must fail closed: code=%d err=%v", code, runErr)
				}
			} else if code != ExitClean || runErr != nil || !strings.Contains(stdout.String(), `"assessment":"NO_FINDINGS"`) ||
				!strings.Contains(stderr.String(), "using finder profile routed") {
				t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, runErr, stdout.String(), stderr.String())
			}
		})
	}
}
