package llm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/bluesky-social/roast/internal/testutil"
)

func TestProviderConversationIdentity(test *testing.T) {
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		test.Run(protocol, func(test *testing.T) {
			var mu sync.Mutex
			attempts := make(map[string]int)
			identities := make(map[string]string)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				defer request.Body.Close()
				body, err := io.ReadAll(request.Body)
				if err != nil {
					test.Error(err)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				marker := "conversation-one"
				if strings.Contains(string(body), "conversation-two") {
					marker = "conversation-two"
				} else if !strings.Contains(string(body), marker) {
					test.Error("request lost conversation prompt")
				}
				sessionID := request.Header.Get("X-Agent-Session-Id")
				if !strings.HasPrefix(sessionID, "roast-") {
					test.Errorf("session ID = %q", sessionID)
				}
				mu.Lock()
				if previous := identities[marker]; previous != "" && previous != sessionID {
					test.Errorf("%s changed identity: %q -> %q", marker, previous, sessionID)
				}
				identities[marker] = sessionID
				attempts[marker]++
				attempt := attempts[marker]
				mu.Unlock()
				writer.Header().Set("Content-Type", "application/json")
				if attempt == 1 {
					writer.Header().Set("Retry-After-Ms", "1")
					writer.WriteHeader(http.StatusInternalServerError)
					_, _ = writer.Write([]byte(`{"error":{"message":"temporary","type":"server_error"}}`))
					return
				}
				tool, arguments := "read", `{"path":"x.go"}`
				if attempt >= 3 {
					tool, arguments = "submit", `{"answer":"done"}`
				}
				var response map[string]any
				switch protocol {
				case "anthropic":
					response = anthropicResponse(tool, arguments)
				case "responses":
					response = map[string]any{
						"id": "resp-test", "object": "response", "status": "completed", "model": "test-model",
						"output": []any{map[string]any{
							"id": "fc-" + tool, "type": "function_call", "call_id": "call-" + tool,
							"name": tool, "arguments": arguments, "status": "completed",
						}},
						"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
					}
				case "chat":
					response = map[string]any{
						"id": "chat-test", "object": "chat.completion", "model": "test-model",
						"choices": []any{map[string]any{
							"index": 0, "finish_reason": "tool_calls",
							"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
								"id": "call-" + tool, "type": "function",
								"function": map[string]any{"name": tool, "arguments": arguments},
							}}},
						}},
						"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
					}
				}
				_ = testutil.WriteInferenceStream(writer, protocol, response)
			}))
			defer server.Close()
			client, err := New(test.Context(), ClientConfig{
				BaseURL: server.URL + "/v1", APIKey: "test", Model: "test-model", Protocol: protocol,
				MaxOutputTokens: 100, MaxAttempts: 2,
			})
			if err != nil {
				test.Fatal(err)
			}
			var conversations sync.WaitGroup
			for _, marker := range []string{"conversation-one", "conversation-two"} {
				conversations.Go(func() {
					var result struct {
						Answer string `json:"answer"`
					}
					err := client.Converse(test.Context(), "system", marker, ConverseOpts{
						MaxTurns: 2, FinalTool: "submit",
						Tools: basicTools(func(context.Context, map[string]any) (any, error) { return "file contents", nil }),
					}, &result)
					if err != nil || result.Answer != "done" {
						test.Errorf("%s: result = %q, error = %v", marker, result.Answer, err)
					}
				})
			}
			conversations.Wait()
			mu.Lock()
			defer mu.Unlock()
			if len(identities) != 2 || identities["conversation-one"] == identities["conversation-two"] {
				test.Fatalf("independent conversations share identities: %v", identities)
			}
			for marker, count := range attempts {
				if count != 3 {
					test.Errorf("%s: attempts = %d, want retry plus two turns", marker, count)
				}
			}
		})
	}
}

func TestStructuredCorrectionsRetainConversationIdentity(test *testing.T) {
	var identities []string
	client := testClient(func(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
		identity, _ := ctx.Value(sessionContextKey{}).(string)
		identities = append(identities, identity)
		arguments := `{"answer":123}`
		if len(identities)%2 == 0 {
			arguments = `{"answer":"done"}`
		}
		return toolCallResponse("submit", arguments), nil
	})
	client.cfg.MaxAttempts = 2
	for range 2 {
		var result struct {
			Answer string `json:"answer"`
		}
		if err := client.CompleteStructured(test.Context(), "system", "user", "submit", &result); err != nil {
			test.Fatal(err)
		}
	}
	if len(identities) != 4 || identities[0] == "" || identities[0] != identities[1] || identities[2] != identities[3] || identities[0] == identities[2] {
		test.Fatalf("conversation identities = %v", identities)
	}
}

type sessionRoundTripper func(*http.Request) (*http.Response, error)

func (transport sessionRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestSessionTransportDoesNotMutateRequests(test *testing.T) {
	transport := &captureTransport{base: sessionRoundTripper(func(request *http.Request) (*http.Response, error) {
		if identity := request.Header.Get("X-Agent-Session-Id"); identity != "roast-test" {
			return nil, fmt.Errorf("session identity = %q", identity)
		}
		return &http.Response{StatusCode: http.StatusNoContent}, nil
	})}
	ctx := context.WithValue(test.Context(), sessionContextKey{}, "roast-test")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.test", nil)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); err != nil {
		test.Fatal(err)
	}
	if identity := request.Header.Get("X-Agent-Session-Id"); identity != "" {
		test.Fatalf("mutated caller request header: %q", identity)
	}
}
