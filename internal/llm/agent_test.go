package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
)

type fakeLanguageModel struct {
	generate func(context.Context, fantasy.Call) (*fantasy.Response, error)
	stream   func(context.Context, fantasy.Call) (fantasy.StreamResponse, error)
}

func (m *fakeLanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return m.generate(ctx, call)
}

func (m *fakeLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.stream != nil {
		return m.stream(ctx, call)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		response, err := m.generate(ctx, call)
		if err != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
			return
		}
		for _, item := range response.Content {
			switch item := item.(type) {
			case fantasy.ToolCallContent:
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: item.ToolCallID, ToolCallName: item.ToolName, ToolCallInput: item.Input}) {
					return
				}
			case fantasy.TextContent:
				for _, part := range []fantasy.StreamPart{
					{Type: fantasy.StreamPartTypeTextStart, ID: "text"},
					{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: item.Text},
					{Type: fantasy.StreamPartTypeTextEnd, ID: "text"},
				} {
					if !yield(part) {
						return
					}
				}
			}
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: response.FinishReason, Usage: response.Usage})
	}, nil
}

func (*fakeLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (*fakeLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

func (*fakeLanguageModel) Provider() string { return "test" }
func (*fakeLanguageModel) Model() string    { return "test-model" }

func testClient(fn func(context.Context, fantasy.Call) (*fantasy.Response, error)) *client {
	return &client{cfg: ClientConfig{Model: "test-model", MaxOutputTokens: 100, MaxAttempts: 1}, model: &fakeLanguageModel{generate: fn}}
}

func toolCallResponse(name, arguments string) *fantasy.Response {
	return &fantasy.Response{
		Content: fantasy.ResponseContent{fantasy.ToolCallContent{
			ToolCallID: "call-" + name, ToolName: name, Input: arguments,
		}},
		FinishReason: fantasy.FinishReasonToolCalls,
		Usage:        fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
}

func textResponse(text string) *fantasy.Response {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: text}},
		FinishReason: fantasy.FinishReasonStop,
	}
}

func basicTools(run ToolFunc) []Tool {
	return []Tool{
		{Name: "read", Schema: map[string]any{"type": "object"}, Run: run},
		{Name: "submit", Schema: map[string]any{"type": "object"}},
	}
}

func TestAgentBudgetRefusalUsesReservedFinalCall(t *testing.T) {
	var calls int
	budgetFired := false
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		tools := call.Tools
		if len(tools) != 1 || tools[0].GetName() != "submit" {
			t.Fatalf("tools = %#v, want final only", tools)
		}
		return toolCallResponse("submit", `{"answer":"budget"}`), nil
	})
	c.cfg.Ledger = NewLedger(1500)
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 4, FinalTool: "submit", OnBudgetExhausted: func() { budgetFired = true },
		Tools: basicTools(func(context.Context, map[string]any) (any, error) { return "read", nil }),
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !budgetFired || calls != 1 || out.Answer != "budget" {
		t.Fatalf("budgetFired=%v calls=%d answer=%q", budgetFired, calls, out.Answer)
	}
}

func TestAgentInFlightDeadlineGetsFreshFinalWindow(t *testing.T) {
	var calls int
	deadlineFired := false
	c := testClient(func(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		tools := call.Tools
		if len(tools) != 1 || tools[0].GetName() != "submit" {
			t.Fatalf("tools = %#v, want final only", tools)
		}
		return toolCallResponse("submit", `{"answer":"deadline"}`), nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 4, FinalTool: "submit", WorkDeadline: time.Now().Add(10 * time.Millisecond), WrapUpWindow: time.Second,
		OnWorkDeadline: func() { deadlineFired = true },
		Tools:          basicTools(func(context.Context, map[string]any) (any, error) { return "read", nil }),
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !deadlineFired || calls != 2 || out.Answer != "deadline" {
		t.Fatalf("deadlineFired=%v calls=%d answer=%q", deadlineFired, calls, out.Answer)
	}
}

func TestInferenceRequestTimeoutIsEndpointFailure(t *testing.T) {
	c := testClient(func(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c.cfg.RequestTimeout = 10 * time.Millisecond
	started := time.Now()
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
	if !errors.Is(err, ErrEndpoint) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want endpoint-wrapped request timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("request timeout took %s", elapsed)
	}
}

func TestAgentNudgesFreeFormResponse(t *testing.T) {
	var calls int
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return textResponse("I should keep looking."), nil
		}
		data, _ := json.Marshal(call.Prompt)
		if !strings.Contains(string(data), nudgeToolName) || !strings.Contains(string(data), "submit") {
			t.Fatalf("second call did not contain nudge result: %s", data)
		}
		return toolCallResponse("submit", `{"answer":"nudged"}`), nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 3, FinalTool: "submit", Tools: basicTools(nil),
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || out.Answer != "nudged" {
		t.Fatalf("calls=%d answer=%q", calls, out.Answer)
	}
}

func TestAgentReportsMalformedFinalArguments(t *testing.T) {
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		return toolCallResponse("submit", `{`), nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 1, FinalTool: "submit", Tools: []Tool{{Name: "submit", Schema: map[string]any{"type": "object"}}},
	}, &out)
	if err == nil || !strings.Contains(err.Error(), "submit arguments did not satisfy the schema") {
		t.Fatalf("error = %v", err)
	}
}

func TestAgentDoesNotRetryFailedDeadlineWrapUp(t *testing.T) {
	var calls int
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		calls++
		return nil, errors.New("transport unavailable")
	})
	c.cfg.MaxAttempts = 3
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 4, FinalTool: "submit", WorkDeadline: time.Now().Add(-time.Second),
		Tools: basicTools(func(context.Context, map[string]any) (any, error) { return "read", nil }),
	}, &out)
	if !errors.Is(err, ErrWorkDeadline) || !errors.Is(err, ErrNoFinalCall) {
		t.Fatalf("error = %v, want deadline and no-final sentinels", err)
	}
	if calls != 1 {
		t.Fatalf("wrap-up calls = %d, want 1", calls)
	}
}

func TestAgentUnknownToolReturnsErrorToModel(t *testing.T) {
	var calls int
	var turns []TurnInfo
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return toolCallResponse("missing", `{}`), nil
		}
		data, _ := json.Marshal(call.Prompt)
		if !strings.Contains(string(data), "unknown tool: missing") {
			t.Fatalf("unknown-tool result missing: %s", data)
		}
		return toolCallResponse("submit", `{"answer":"recovered"}`), nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 3, FinalTool: "submit", OnTurn: func(info TurnInfo) { turns = append(turns, info) },
		Tools: []Tool{{Name: "submit", Schema: map[string]any{"type": "object"}}},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || out.Answer != "recovered" || len(turns) != 1 || turns[0].Tool != "missing" {
		t.Fatalf("calls=%d answer=%q turns=%#v", calls, out.Answer, turns)
	}
}

func TestAgentExecutesMultipleToolsSequentially(t *testing.T) {
	var calls int
	var order []string
	c := testClient(func(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return &fantasy.Response{
				Content: fantasy.ResponseContent{
					fantasy.ToolCallContent{ToolCallID: "call-first", ToolName: "first", Input: `{}`},
					fantasy.ToolCallContent{ToolCallID: "call-second", ToolName: "second", Input: `{}`},
				},
				FinishReason: fantasy.FinishReasonToolCalls,
			}, nil
		}
		return toolCallResponse("submit", `{"answer":"done"}`), nil
	})
	tool := func(name string) Tool {
		return Tool{Name: name, Schema: map[string]any{"type": "object"}, Run: func(context.Context, map[string]any) (any, error) {
			order = append(order, name)
			return name, nil
		}}
	}
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 3, FinalTool: "submit", Tools: []Tool{tool("first"), tool("second"), {Name: "submit", Schema: map[string]any{"type": "object"}}},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "first,second" {
		t.Fatalf("tool order = %v", order)
	}
}

func TestFantasyRetriesTransientModelFailure(t *testing.T) {
	var calls atomic.Int32
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		if calls.Add(1) == 1 {
			return nil, &fantasy.ProviderError{Message: "temporary transport failure", TransientError: true, ResponseHeaders: map[string]string{"retry-after-ms": "1"}}
		}
		return toolCallResponse("submit", `{"answer":"retried"}`), nil
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Answer string `json:"answer"`
	}
	if err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || out.Answer != "retried" {
		t.Fatalf("calls=%d answer=%q", calls.Load(), out.Answer)
	}
}

func TestStructuredOutputRetryUsesFantasyLoop(t *testing.T) {
	var calls int
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return toolCallResponse("verdict", `{"count":"wrong"}`), nil
		}
		data, _ := json.Marshal(call.Prompt)
		if !strings.Contains(string(data), "not valid JSON matching the schema") {
			t.Fatalf("retry input lacks validation feedback: %s", data)
		}
		return toolCallResponse("verdict", `{"count":2}`), nil
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Count int `json:"count"`
	}
	if err := c.CompleteStructured(t.Context(), "system", "user", "verdict", &out); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || out.Count != 2 {
		t.Fatalf("calls=%d output=%#v", calls, out)
	}
}

func TestStructuredOutputRetryDoesNotMaskLaterProviderFailure(t *testing.T) {
	var calls int
	c := testClient(func(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return toolCallResponse("verdict", `{"count":"wrong"}`), nil
		}
		return nil, errors.New("provider unavailable")
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Count int `json:"count"`
	}
	err := c.CompleteStructured(t.Context(), "system", "user", "verdict", &out)
	if !errors.Is(err, ErrEndpoint) || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("error = %v, want endpoint failure", err)
	}
	if strings.Contains(err.Error(), "did not satisfy the schema") {
		t.Fatalf("stale validation error masked endpoint failure: %v", err)
	}
}

func TestGenericUsageAccounting(t *testing.T) {
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		resp := toolCallResponse("submit", `{"answer":"done"}`)
		resp.Usage = fantasy.Usage{InputTokens: 8, CacheReadTokens: 4, CacheCreationTokens: 1, OutputTokens: 3, TotalTokens: 11}
		return resp, nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	if err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out); err != nil {
		t.Fatal(err)
	}
	if got := c.Usage(); got.Calls != 1 || got.UncachedInputTokens != 3 || got.CachedInputTokens != 4 || got.CacheWriteTokens != 1 || got.OutputTokens != 3 {
		t.Fatalf("usage = %#v", got)
	}
}

func TestAgentRestoresCompleteToolSchemaAtModelBoundary(t *testing.T) {
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		if len(call.Tools) != 1 {
			t.Fatalf("tools = %#v", call.Tools)
		}
		tool, ok := call.Tools[0].(fantasy.FunctionTool)
		if !ok {
			t.Fatalf("tool type = %T", call.Tools[0])
		}
		if additional, ok := tool.InputSchema["additionalProperties"].(bool); !ok || additional {
			t.Fatalf("input schema = %#v", tool.InputSchema)
		}
		return toolCallResponse("submit", `{"answer":"done"}`), nil
	})
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 1, FinalTool: "submit", Tools: []Tool{{Name: "submit", Schema: map[string]any{
			"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}},
			"required": []string{"answer"}, "additionalProperties": false,
		}}},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
}

func TestFinalToolPreservesLargeIntegers(t *testing.T) {
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		return toolCallResponse("submit", `{"id":9007199254740993}`), nil
	})
	var out struct {
		ID int64 `json:"id"`
	}
	if err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 1, FinalTool: "submit", Tools: []Tool{{Name: "submit", Schema: map[string]any{"type": "object"}}},
	}, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != 9007199254740993 {
		t.Fatalf("id = %d", out.ID)
	}
}
