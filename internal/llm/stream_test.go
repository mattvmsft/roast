package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
)

func TestStreamAbnormalFinishNeverDispatchesTools(t *testing.T) {
	for _, reason := range []fantasy.FinishReason{fantasy.FinishReasonLength, fantasy.FinishReasonContentFilter, fantasy.FinishReasonError, fantasy.FinishReasonUnknown} {
		t.Run(string(reason), func(t *testing.T) {
			c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
				return &fantasy.Response{
					Content: fantasy.ResponseContent{
						fantasy.ToolCallContent{ToolCallID: "read", ToolName: "read", Input: `{}`},
						fantasy.ToolCallContent{ToolCallID: "submit", ToolName: "submit", Input: `{"answer":"must not succeed"}`},
					},
					FinishReason: reason,
				}, nil
			})
			var tools int
			var out struct{ Answer string }
			err := c.Converse(t.Context(), "system", "user", ConverseOpts{
				MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
					tools++
					return "read", nil
				}),
			}, &out)
			if !errors.Is(err, ErrNoFinalCall) || tools != 0 || out.Answer != "" {
				t.Fatalf("error=%v tools=%d answer=%q", err, tools, out.Answer)
			}
		})
	}
}

func TestStreamFinalSubmissionSuppressesOtherToolCalls(t *testing.T) {
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		response := toolCallResponse("submit", `{"answer":"done"}`)
		response.Content = append(fantasy.ResponseContent{fantasy.ToolCallContent{ToolCallID: "read", ToolName: "read", Input: `{}`}}, response.Content...)
		return response, nil
	})
	var out struct{ Answer string }
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 2, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
			t.Error("exploration tool ran alongside final submission")
			return nil, nil
		}),
	}, &out)
	if err != nil || out.Answer != "done" {
		t.Fatalf("error=%v answer=%q", err, out.Answer)
	}
}

func TestStreamCancellationDoesNotDispatchBufferedTool(t *testing.T) {
	for _, workDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "request-timeout", true: "work-deadline"}[workDeadline], func(t *testing.T) {
			c := testClient(nil)
			calls := 0
			c.model.(*fakeLanguageModel).stream = func(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
				calls++
				return func(yield func(fantasy.StreamPart) bool) {
					if calls == 1 {
						if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "read", ToolCallName: "read", ToolCallInput: `{}`}) {
							return
						}
						<-ctx.Done()
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
						return
					}
					if len(call.Tools) != 1 || call.Tools[0].GetName() != "submit" || ctx.Err() != nil {
						t.Error("wrap-up did not get final-only tools and a fresh context")
					}
					if yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "submit", ToolCallName: "submit", ToolCallInput: `{"answer":"partial"}`}) {
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
					}
				}, nil
			}
			opts := ConverseOpts{
				MaxTurns: 4, FinalTool: "submit", WrapUpWindow: time.Second,
				Tools: basicTools(func(context.Context, map[string]any) (any, error) {
					t.Error("buffered exploration tool ran after cancellation")
					return nil, nil
				}),
			}
			if workDeadline {
				opts.WorkDeadline = time.Now().Add(100 * time.Millisecond)
			} else {
				c.cfg.RequestTimeout = 20 * time.Millisecond
			}
			var out struct{ Answer string }
			err := c.Converse(t.Context(), "system", "user", opts, &out)
			if workDeadline {
				if err != nil || calls != 2 || out.Answer != "partial" {
					t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
				}
			} else if !errors.Is(err, ErrEndpoint) || calls != 1 || out.Answer != "" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
		})
	}
}

func TestStreamConsumerStopCancelsAndReleasesReservation(t *testing.T) {
	c := testClient(nil)
	c.cfg.Ledger = NewLedger(100000)
	var providerCtx context.Context
	c.model.(*fakeLanguageModel).stream = func(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		providerCtx = ctx
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "partial"})
		}, nil
	}
	managed := &managedModel{client: c, state: &agentRunState{wd: newWorkDeadlineState(ConverseOpts{})}}
	stream, err := managed.Stream(t.Context(), fantasy.Call{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
		break
	}
	if providerCtx == nil || providerCtx.Err() != context.Canceled || c.cfg.Ledger.outstanding != 0 {
		t.Fatalf("provider context=%v outstanding=%v", providerCtx, c.cfg.Ledger.outstanding)
	}
}
