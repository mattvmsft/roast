package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/roast/internal/testutil"
)

// Exercise the real Fantasy/Anthropic SDK path, not a fake LanguageModel. The
// production-sized ceiling must reach the native endpoint unchanged even though
// the fixture produces only a few tokens. Either streaming or ordinary Messages
// requests are acceptable; compatibility endpoints and smaller budgets are not.
func TestAnthropicNativeMessagesPreservesOutputBudget(t *testing.T) {
	for _, budget := range []int{1024, 32768} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			var calls, toolCalls atomic.Int32
			var captured []RawResponse
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()
				call := calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "fixture-key" || r.Header.Get("Anthropic-Version") == "" {
					t.Error("request did not use the native authenticated Messages endpoint")
					http.Error(w, "native Messages required", http.StatusBadRequest)
					return
				}
				var request struct {
					Model      string            `json:"model"`
					MaxTokens  int               `json:"max_tokens"`
					Stream     bool              `json:"stream"`
					System     json.RawMessage   `json:"system"`
					Messages   json.RawMessage   `json:"messages"`
					Tools      []json.RawMessage `json:"tools"`
					ToolChoice struct {
						Type string `json:"type"`
					} `json:"tool_choice"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode native request: %v", err)
					http.Error(w, "invalid JSON", http.StatusBadRequest)
					return
				}
				if request.MaxTokens != budget || request.Model != "claude-test" || len(request.System) == 0 || len(request.Tools) == 0 {
					t.Errorf("native request lost its budget/model/system/tools: %+v", request)
					http.Error(w, "request configuration changed", http.StatusBadRequest)
					return
				}
				if request.ToolChoice.Type != "auto" {
					http.Error(w, "forced tool choice is not supported", http.StatusBadRequest)
					return
				}
				tool, arguments := "read", `{"path":"fixture.go"}`
				if call > 1 {
					if !strings.Contains(string(request.Messages), "tool_result") || !strings.Contains(string(request.Messages), "package fixture") {
						t.Error("native follow-up request lost the tool result")
					}
					tool, arguments = "submit", `{"answer":"done"}`
				}
				if err := writeAnthropicBudgetResponse(w, request.Stream, tool, arguments); err != nil {
					t.Errorf("write native fixture response: %v", err)
				}
			}))
			defer server.Close()

			c, err := New(t.Context(), ClientConfig{
				BaseURL: server.URL + "/v1", APIKey: "fixture-key", Model: "claude-test",
				Protocol: "anthropic", MaxOutputTokens: budget, MaxAttempts: 1,
				RequestTimeout: 10 * time.Minute,
				CaptureResponse: func(response RawResponse) error {
					captured = append(captured, response)
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var out struct {
				Answer string `json:"answer"`
			}
			err = c.Converse(t.Context(), "system", "user", ConverseOpts{
				MaxTurns: 2, FinalTool: "submit",
				Tools: []Tool{
					{Name: "read", Description: "read a file", Schema: map[string]any{"type": "object"}, Run: func(context.Context, map[string]any) (any, error) {
						toolCalls.Add(1)
						return map[string]any{"content": "package fixture"}, nil
					}},
					{Name: "submit", Description: "return the result", Schema: map[string]any{
						"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}},
						"required": []string{"answer"},
					}},
				},
			}, &out)
			if err != nil {
				t.Fatalf("native Anthropic run with max_tokens=%d failed after %d HTTP requests: %v", budget, calls.Load(), err)
			}
			if calls.Load() != 2 || toolCalls.Load() != 1 || out.Answer != "done" {
				t.Fatalf("native run incomplete: HTTP requests=%d tool calls=%d answer=%q", calls.Load(), toolCalls.Load(), out.Answer)
			}
			if len(captured) != 2 || c.Usage().Calls != 2 {
				t.Fatalf("captured=%d usage=%+v", len(captured), c.Usage())
			}
			for _, response := range captured {
				if response.ContentType != "text/event-stream" || !strings.Contains(string(response.Body), "event: message_stop") {
					t.Fatalf("native stream was not captured intact: %+v", response)
				}
			}
		})
	}
}

// Keep the acceptance test independent of the eventual timeout/streaming fix.
func writeAnthropicBudgetResponse(w http.ResponseWriter, stream bool, tool, arguments string) error {
	response := anthropicResponse(tool, arguments)
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(w).Encode(response)
	}
	return testutil.WriteInferenceStream(w, "anthropic", response)
}
