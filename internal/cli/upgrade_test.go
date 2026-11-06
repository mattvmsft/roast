package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/runbundle"
	"github.com/bluesky-social/roast/internal/runupload"
	"github.com/bluesky-social/roast/internal/testutil"
)

// Exercise the real CLI enrollment, both model roles, durable records, and
// automatic presigned upload without touching a user's installation/services.
func TestLegacyUpgradeThroughConfigurationURLAndPresignedUpload(t *testing.T) {
	for _, inPlace := range []bool{true, false} {
		t.Run(fmt.Sprintf("in-place=%v", inPlace), func(t *testing.T) {
			for _, name := range []string{
				"ROAST_FINDER_BASE_URL", "ROAST_VERIFIER_BASE_URL", "ROAST_FINDER_MODEL", "ROAST_VERIFIER_MODEL",
				"ROAST_FINDER_API_KEY", "ROAST_VERIFIER_API_KEY", "ROAST_FINDER_PROTOCOL", "ROAST_VERIFIER_PROTOCOL",
				"OPENAI_BASE_URL", "OPENAI_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ROAST_AUTHOR_MODEL",
			} {
				t.Setenv(name, "")
			}
			t.Setenv("ROAST_NO_HOOK", "1")
			t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			recordDir := t.TempDir()
			t.Setenv("ROAST_RECORD_DIR", recordDir)
			legacyPath, err := config.LegacyUserPath()
			if err != nil {
				t.Fatal(err)
			}
			legacy := []byte(`gateway:
  api_key: legacy-upgrade-key
roles:
  finder:
    model: old-finder
    api: chat
gate:
  auto_install: false
sourcebot:
  url: https://retired-search.example
  token: discarded-retrieval-key
upload:
  enabled: true
`)
			if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(legacyPath, legacy, 0o600); err != nil {
				t.Fatal(err)
			}
			destination := legacyPath
			if !inPlace {
				destination = filepath.Join(t.TempDir(), "roast", "config.yaml")
			}

			var modelCalls, grants, completions atomic.Int32
			models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer legacy-upgrade-key" || r.URL.Path != "/v1/chat/completions" {
					http.Error(w, "incorrect migrated inference request", http.StatusBadRequest)
					return
				}
				var request struct{ Model string }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				name, arguments := "findings", `{"assessment":"FINDINGS_PRESENT","findings":[{"reasoning":"unconditional panic","severity":"HIGH","confidence":0.95,"description":"The function always panics.","file":"bad.go","lines":[3],"code_quote":"panic(\"oops\")","fixed_code":"return"}]}`
				switch request.Model {
				case "finder-test":
				case "verifier-test":
					name = "submit_verdict"
					arguments = `{"reasoning":"unconditional panic","status":"confirmed","confidence":0.99,"grounding":"bad.go line 3 calls panic unconditionally","recalibrated_severity":""}`
				default:
					http.Error(w, "unexpected model", http.StatusBadRequest)
					return
				}
				modelCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = testutil.WriteJSONStream(w, "chat", fmt.Sprintf(`{"id":"upgrade","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, request.Model, name, arguments))
			}))
			defer models.Close()

			var uploadMu sync.Mutex
			var uploaded []byte
			storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.Header.Get("Authorization") != "" {
					http.Error(w, "broker bearer must not reach object storage", http.StatusBadRequest)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				uploadMu.Lock()
				uploaded = body
				uploadMu.Unlock()
				w.Header().Set("ETag", `"uploaded"`)
			}))
			defer storage.Close()
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer legacy-upgrade-key" || r.Method != http.MethodPost {
					http.Error(w, "incorrect migrated broker credential", http.StatusBadRequest)
					return
				}
				if r.URL.Path == "/api/v1/roast/runs" {
					var request struct {
						RunID  string `json:"run_id"`
						Digest string `json:"bundle_sha256"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						return
					}
					grants.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"run_id": request.RunID, "attempt_id": "attempt", "method": "PUT", "upload_url": storage.URL,
						"object_key": "runs/sha256/" + request.Digest + ".tar.zst",
					})
				} else if strings.HasSuffix(r.URL.Path, "/complete") {
					completions.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]string{"status": "uploaded"})
				} else {
					http.NotFound(w, r)
				}
			}))
			defer control.Close()
			configuration := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("configuration URL received a credential")
				}
				fmt.Fprintf(w, `version: 1
provisioning:
  import_legacy_credential: true
inference_profiles:
  finder:
    endpoint: %s/v1
    model: finder-test
    protocol: chat
  verifier:
    endpoint: %s/v1
    model: verifier-test
    protocol: chat
roles:
  finder:
    profile: finder
  verifier:
    profile: verifier
upload:
  enabled: true
  backend: presigned
  endpoint: %s
`, models.URL, models.URL, control.URL)
			}))
			defer configuration.Close()

			var output bytes.Buffer
			code, err := ExecuteContext(t.Context(), []string{"config", "init", "--from", configuration.URL, "--output", destination}, &output, io.Discard)
			if err != nil || code != ExitClean {
				t.Fatalf("configuration initialization: code=%d err=%v", code, err)
			}
			preserved := legacyPath
			if inPlace {
				preserved += ".pre-v1"
			}
			backup, err := os.ReadFile(preserved)
			if err != nil || !bytes.Equal(backup, legacy) {
				t.Fatalf("legacy config not preserved: %v", err)
			}
			current, err := os.ReadFile(destination)
			if err != nil || bytes.Contains(current, []byte("discarded-retrieval-key")) || bytes.Contains(current, []byte("auto_install")) {
				t.Fatalf("retired configuration survived provisioning: %v", err)
			}

			repo := initChangedRepo(t)
			previous, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(repo); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(previous) })
			output.Reset()
			code, err = ExecuteContext(t.Context(), []string{"review", "--config", destination, "--format", "json", "--max-severity", "HIGH", "--max-attempts", "1"}, &output, io.Discard)
			if err != nil || code != ExitFindings || modelCalls.Load() != 2 || grants.Load() != 1 || completions.Load() != 1 {
				t.Fatalf("upgrade review: code=%d err=%v model calls=%d grants=%d completions=%d output=%s", code, err, modelCalls.Load(), grants.Load(), completions.Load(), output.String())
			}
			var report struct {
				RunID string `json:"run_id"`
			}
			if err := json.Unmarshal(output.Bytes(), &report); err != nil || report.RunID == "" {
				t.Fatalf("review did not produce a durable run: %v", err)
			}
			runDir := filepath.Join(recordDir, "runs", report.RunID)
			if _, err := runbundle.Verify(filepath.Join(runDir, runbundle.BundleFile)); err != nil {
				t.Fatalf("invalid upgraded run bundle: %v", err)
			}
			bundle, err := os.ReadFile(filepath.Join(runDir, runbundle.BundleFile))
			if err != nil {
				t.Fatal(err)
			}
			uploadMu.Lock()
			matches := bytes.Equal(bundle, uploaded)
			uploadMu.Unlock()
			if !matches {
				t.Fatal("automatic upload did not send the durable run bundle")
			}
			statuses, err := runupload.ListStatus(recordDir)
			if err != nil || len(statuses) != 1 || statuses[0].Status != "uploaded" {
				t.Fatalf("upload status: %v err=%v", statuses, err)
			}
			digest := sha256.Sum256(bundle)
			if !strings.Contains(statuses[0].Detail, hex.EncodeToString(digest[:])) {
				t.Fatal("uploaded object is not content addressed")
			}
			meta, err := os.ReadFile(filepath.Join(runDir, record.MetaFile))
			if err != nil || bytes.Contains(meta, []byte("legacy-upgrade-key")) || bytes.Contains(meta, []byte("discarded-retrieval-key")) {
				t.Fatalf("credential leaked into run metadata: %v", err)
			}
		})
	}
}
