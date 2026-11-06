package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestDoctorEndpointCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	result := doctorEndpointCheck(t.Context(), server.Client(), "finder", config.ResolvedRole{
		BaseURL: server.URL + "/v1", APIKey: "secret", Protocol: "responses",
	})
	if result.Status != doctorPass || strings.Contains(result.Detail, "secret") {
		t.Fatalf("doctorEndpointCheck() = %#v", result)
	}
}

func TestDoctorJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		_ = testutil.WriteInferenceStream(w, "responses", map[string]any{
			"id": "resp-doctor", "object": "response", "created_at": 1,
			"status": "completed", "model": "doctor-model",
			"output": []any{map[string]any{
				"id": "fc-doctor", "type": "function_call", "call_id": "call-doctor",
				"name": "doctor_probe", "arguments": `{"ok":true}`, "status": "completed",
			}},
			"usage": map[string]any{
				"input_tokens": 10, "output_tokens": 3, "total_tokens": 13,
				"input_tokens_details":  map[string]any{"cached_tokens": 0},
				"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			},
		})
	}))
	defer server.Close()
	t.Setenv("DOCTOR_KEY", "secret")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	body := "version: 1\ninference_profiles:\n  finder:\n    endpoint: " + server.URL + "/v1\n    api_key_env: DOCTOR_KEY\n    model: finder-model\n    protocol: responses\n  verifier:\n    endpoint: " + server.URL + "/v1\n    api_key_env: DOCTOR_KEY\n    model: verifier-model\n    protocol: responses\nroles:\n  finder:\n    profile: finder\n  verifier:\n    profile: verifier\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code, err := ExecuteContext(t.Context(), []string{"doctor", "--json", "--config", configPath}, &stdout, &stderr)
	if err != nil || code != ExitClean {
		t.Fatalf("doctor code=%d err=%v stdout=%s stderr=%s", code, err, stdout.String(), stderr.String())
	}
	var results []doctorResult
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatalf("decode results: %v: %s", err, stdout.String())
	}
	if len(results) < 8 {
		t.Fatalf("doctor returned only %d results: %#v", len(results), results)
	}
	if strings.Contains(stdout.String(), "secret") {
		t.Fatal("doctor output exposed a credential")
	}
	sources := map[string]doctorResult{}
	for _, result := range results {
		sources[result.Name] = result
	}
	for _, role := range []string{"finder", "verifier"} {
		if got := sources[role+"-credential"]; got.Status != doctorPass || got.Detail != "DOCTOR_KEY (profile api_key_env)" {
			t.Errorf("%s credential = %#v", role, got)
		}
	}
}

func TestDoctorReportsWithheldAmbientCredential(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "anthropic-secret" {
			t.Errorf("ambient pairing did not reach its own base URL: %s", r.URL.Path)
		}
		http.Error(w, "probe only", http.StatusServiceUnavailable)
	}))
	defer gateway.Close()
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
	t.Setenv("ANTHROPIC_BASE_URL", gateway.URL+"/v1")
	t.Setenv("ROAST_NO_UPDATE_CHECK", "1")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	body := "version: 1\ninference_profiles:\n  finder:\n    endpoint: https://template.example/v1\n    model: finder-model\n  verifier:\n    model: verifier-model\n    protocol: anthropic\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	results := doctorChecks(t.Context(), configPath)
	byName := map[string]doctorResult{}
	for _, result := range results {
		byName[result.Name] = result
	}
	finder := byName["finder-credential"]
	if finder.Status != doctorFail || !strings.Contains(finder.Detail, "OPENAI_API_KEY is set but is sent only to its provider's default endpoint or OPENAI_BASE_URL") {
		t.Fatalf("finder credential = %#v", finder)
	}
	if profile := byName["finder-profile"]; profile.Status != doctorFail || !strings.Contains(profile.Detail, "is not sent to https://template.example/v1") {
		t.Fatalf("finder profile = %#v", profile)
	}
	if verifier := byName["verifier-credential"]; verifier.Status != doctorPass || verifier.Detail != "ANTHROPIC_API_KEY (ambient provider key)" {
		t.Fatalf("verifier credential = %#v", verifier)
	}
	if _, probed := byName["finder-endpoint"]; probed {
		t.Fatal("doctor probed an endpoint without a credential")
	}
	for _, result := range results {
		if strings.Contains(result.Detail, "secret") {
			t.Fatalf("doctor exposed a credential: %#v", result)
		}
	}
}

func TestDoctorRoundtripUsesConfiguredResponsesRoute(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = testutil.WriteJSONStream(w, "responses", `{"id":"resp","object":"response","created_at":1,"status":"completed","model":"finder-model","output":[{"id":"fc","type":"function_call","call_id":"call","name":"doctor_probe","arguments":"{\"ok\":true}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`)
	}))
	defer server.Close()
	result := doctorRoundtripCheck(t.Context(), "finder", config.ResolvedRole{
		BaseURL: server.URL + "/v1", APIKey: "secret", Model: "finder-model",
		Protocol: "responses", MaxOutputTokens: 1024,
	})
	if result.Status != doctorPass || calls != 1 {
		t.Fatalf("doctorRoundtripCheck() = %#v, calls=%d", result, calls)
	}
}
