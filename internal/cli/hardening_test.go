package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/testutil"
)

// TestReviewPinsCorpusWithoutSnapshot: a review whose replayable snapshot
// cannot be recorded — no commits yet, or ROAST_SNAPSHOT=0 — must still read
// one pinned tree. Before, both fell back to the live working tree, where
// read_file served .git/config and later edits changed the evidence.
func TestReviewPinsCorpusWithoutSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot string
		commit   bool
	}{
		{"no HEAD", "1", false},
		{"ROAST_SNAPSHOT=0", "0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROAST_RECORD_DIR", t.TempDir())
			t.Setenv("ROAST_SNAPSHOT", tc.snapshot)
			repo := t.TempDir()
			runGit(t, repo, "init")
			runGit(t, repo, "config", "user.email", "test@example.com")
			runGit(t, repo, "config", "user.name", "Test")
			runGit(t, repo, "config", "remote.origin.url", "https://user:FAKETOKEN@example.com/x/y")
			bad := filepath.Join(repo, "bad.go")
			if tc.commit {
				if err := os.WriteFile(bad, []byte("package example\n\nfunc bad() {}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, repo, "add", "bad.go")
				runGit(t, repo, "commit", "-m", "initial")
			}
			if err := os.WriteFile(bad, []byte("package example\n\nfunc bad() { panic(\"oops\") }\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			var mu sync.Mutex
			verifierCalls := 0
			var failures []string
			fail := func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				failures = append(failures, fmt.Sprintf(format, args...))
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { _ = r.Body.Close() }()
				var request struct {
					Model    string          `json:"model"`
					Messages json.RawMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				messages := string(request.Messages)
				var name, arguments string
				switch request.Model {
				case "finder-test":
					// Edit the checkout mid-review; the pinned corpus must not move.
					if err := os.WriteFile(bad, []byte("package example\n\nfunc bad() {}\n"), 0o600); err != nil {
						fail("mutate: %v", err)
					}
					name = "findings"
					arguments = `{"assessment":"FINDINGS_PRESENT","findings":[{"reasoning":"panic is reachable","severity":"high","confidence":0.95,"cwe":"","description":"The function always panics.","file":"bad.go","lines":[3],"code_quote":"panic(\"oops\")","fixed_code":"return","frontend_category":""}]}`
				case "verifier-test":
					mu.Lock()
					verifierCalls++
					call := verifierCalls
					mu.Unlock()
					// The snippet is JSON inside the request, so its quotes are escaped.
					if !strings.Contains(messages, `func bad() { panic(\"oops\") }`) {
						fail("verifier prompt drifted off the pinned tree: %.300s", messages)
					}
					if call == 1 {
						name, arguments = "read_file", `{"path":".git/config"}`
						break
					}
					if strings.Contains(messages, "FAKETOKEN") || strings.Contains(messages, "repositoryformatversion") || !strings.Contains(messages, `"role":"tool"`) {
						fail("read_file served .git/config, or no tool result came back: %.300s", messages)
					}
					name = "submit_verdict"
					arguments = `{"reasoning":"unconditional","status":"confirmed","confidence":0.99,"grounding":"bad.go line 3 panics","recalibrated_severity":""}`
				default:
					http.Error(w, "unexpected model", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = testutil.WriteJSONStream(w, "chat", fmt.Sprintf(`{"id":"test","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, request.Model, name, arguments))
			}))
			defer server.Close()

			configPath := filepath.Join(t.TempDir(), "config.yaml")
			configBody := fmt.Sprintf(`version: 1
inference_profiles:
  finder-test:
    endpoint: %s/v1
    api_key: test-key
    model: finder-test
    protocol: chat
    max_output_tokens: 2048
  verifier-test:
    endpoint: %s/v1
    api_key: test-key
    model: verifier-test
    protocol: chat
    max_output_tokens: 2048
roles:
  finder:
    profile: finder-test
  verifier:
    profile: verifier-test
`, server.URL, server.URL)
			if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
				t.Fatal(err)
			}
			old, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(repo); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(old) })

			var stdout, stderr bytes.Buffer
			code, runErr := ExecuteContext(t.Context(), []string{
				"review", "--config", configPath, "--format", "json", "--max-severity", "HIGH",
				"--deadline=0", "--request-timeout=0", "--max-attempts=1",
			}, &stdout, &stderr)
			mu.Lock()
			defer mu.Unlock()
			for _, f := range failures {
				t.Error(f)
			}
			// The lowercase "high" also exercises the severity gate.
			if runErr != nil || code != ExitFindings {
				t.Fatalf("code=%d err=%v stdout=%s stderr=%s", code, runErr, stdout.String(), stderr.String())
			}
			if verifierCalls != 2 {
				t.Errorf("verifier calls = %d, want a read_file turn and a verdict", verifierCalls)
			}
			if !strings.Contains(stdout.String(), `"severity":"HIGH"`) {
				t.Errorf("severity was not canonicalized: %s", stdout.String())
			}
		})
	}
}

// TestRunsShowEscapesStoredReports: a report written before render escaped
// finding text can hold raw control bytes; showing it must not replay them.
func TestRunsShowEscapesStoredReports(t *testing.T) {
	t.Setenv("ROAST_RECORD_DIR", t.TempDir())
	runID := "20261031T000000Z-0123abcd"
	if _, err := record.WriteReportRequired(runID, "### R-1 [HIGH] x\x1b]0;pwned\x07\x1b[2J\n- **Reasoning:** \u009b31m\n"); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"runs", "show", runID}, &stdout, &bytes.Buffer{})
	if err != nil || code != ExitClean {
		t.Fatalf("code=%d err=%v", code, err)
	}
	out := stdout.String()
	if strings.ContainsAny(out, "\x1b\x07\u009b") {
		t.Errorf("runs show replayed control bytes: %q", out)
	}
	if !strings.Contains(out, `\x1b]0;pwned\x07`) {
		t.Errorf("escapes must stay visible: %q", out)
	}
}

func TestWriteErrorEscapesControls(t *testing.T) {
	var buf bytes.Buffer
	WriteError(&buf, ExitPartial, fmt.Errorf("git diff: fatal: bad path a\x1b[2J.go"))
	if strings.Contains(buf.String(), "\x1b") || !strings.Contains(buf.String(), `a\x1b[2J.go`) {
		t.Errorf("WriteError = %q", buf.String())
	}
}
