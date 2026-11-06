package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

func assertAutomaticFinalCall(t *testing.T, call fantasy.Call) {
	t.Helper()
	if call.ToolChoice == nil || *call.ToolChoice != fantasy.ToolChoiceAuto {
		t.Fatalf("tool choice = %v, want auto", call.ToolChoice)
	}
	if len(call.Tools) != 1 || call.Tools[0].GetName() != "submit" {
		t.Fatalf("tools = %#v, want submit only", call.Tools)
	}
}

func TestFinalizationUsesAutomaticToolChoice(t *testing.T) {
	for _, mode := range []string{"structured", "turn-limit", "budget", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
				calls++
				if calls > 5 {
					t.Fatal("valid submission did not terminate finalization")
				}
				assertAutomaticFinalCall(t, call)
				return toolCallResponse("submit", `{"answer":"done"}`), nil
			})
			var out struct {
				Answer string `json:"answer"`
			}
			opts := ConverseOpts{MaxTurns: 1, FinalTool: "submit", Tools: basicTools(nil)}
			if mode == "budget" {
				opts.MaxTurns = 4
				c.cfg.Ledger = NewLedger(1500)
			}
			if mode == "deadline" {
				opts.MaxTurns = 4
				opts.WorkDeadline = time.Now().Add(-time.Second)
			}
			var err error
			if mode == "structured" {
				err = c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			} else {
				err = c.Converse(t.Context(), "system", "user", opts, &out)
			}
			if err != nil || calls != 1 || out.Answer != "done" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
		})
	}
}

func TestFinalizationRemindsWithoutReopeningExploration(t *testing.T) {
	for _, response := range []string{"prose", "exploration-tool", "unknown-tool"} {
		t.Run(response, func(t *testing.T) {
			calls := 0
			c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
				calls++
				assertAutomaticFinalCall(t, call)
				if calls == 1 {
					switch response {
					case "exploration-tool":
						return toolCallResponse("read", `{}`), nil
					case "unknown-tool":
						return toolCallResponse("invented", `{}`), nil
					default:
						return textResponse(`{"answer":"not a submission"}`), nil
					}
				}
				prompt, _ := json.Marshal(call.Prompt)
				if !strings.Contains(string(prompt), "submit") || !strings.Contains(string(prompt), "tool call") {
					t.Fatalf("retry lacks submission reminder: %s", prompt)
				}
				return toolCallResponse("submit", `{"answer":"done"}`), nil
			})
			c.cfg.MaxAttempts = 2
			var out struct {
				Answer string `json:"answer"`
			}
			err := c.Converse(t.Context(), "system", "user", ConverseOpts{
				MaxTurns: 1, FinalTool: "submit", Tools: basicTools(func(context.Context, map[string]any) (any, error) {
					t.Error("exploration ran after finalization")
					return nil, nil
				}),
			}, &out)
			if err != nil || calls != 2 || out.Answer != "done" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
		})
	}
}

func TestFinalizationMissingSubmissionIsBounded(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(map[bool]string{false: "agent", true: "structured"}[structured], func(t *testing.T) {
			calls := 0
			c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
				calls++
				if calls > 3 {
					t.Fatal("unbounded finalization")
				}
				return textResponse(`{"answer":"not a tool call"}`), nil
			})
			c.cfg.MaxAttempts = 3
			var out struct {
				Answer string `json:"answer"`
			}
			var err error
			if structured {
				err = c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			} else {
				err = c.Converse(t.Context(), "system", "user", ConverseOpts{MaxTurns: 1, FinalTool: "submit", Tools: basicTools(nil)}, &out)
			}
			if !errors.Is(err, ErrNoFinalCall) || calls != 3 || out.Answer != "" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
		})
	}
}

func TestFinalizationDoesNotRetryAfterCutoff(t *testing.T) {
	for _, cutoff := range []string{"budget", "deadline"} {
		t.Run(cutoff, func(t *testing.T) {
			calls := 0
			c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
				calls++
				return textResponse("no submission"), nil
			})
			c.cfg.MaxAttempts = 3
			opts := ConverseOpts{MaxTurns: 4, FinalTool: "submit", Tools: basicTools(nil)}
			sentinel := ErrBudgetExhausted
			if cutoff == "budget" {
				c.cfg.Ledger = NewLedger(1500)
			} else {
				opts.WorkDeadline = time.Now().Add(-time.Second)
				sentinel = ErrWorkDeadline
			}
			var out struct {
				Answer string `json:"answer"`
			}
			err := c.Converse(t.Context(), "system", "user", opts, &out)
			if !errors.Is(err, sentinel) || !errors.Is(err, ErrNoFinalCall) || calls != 1 || out.Answer != "" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
		})
	}
}

func TestFinalizationSharesStructuredRetryAllowance(t *testing.T) {
	for _, proseFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid-then-prose", true: "prose-then-invalid"}[proseFirst], func(t *testing.T) {
			calls := 0
			c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
				calls++
				if calls > 2 {
					t.Fatal("submission retries exceeded the shared allowance")
				}
				if (calls == 1) == proseFirst {
					return textResponse(`{"answer":"not a tool call"}`), nil
				}
				return toolCallResponse("submit", `{"answer":42}`), nil
			})
			c.cfg.MaxAttempts = 2
			var out struct {
				Answer string `json:"answer"`
			}
			err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
			if err == nil || calls != 2 || out.Answer != "" {
				t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
			}
			if !proseFirst && !errors.Is(err, ErrNoFinalCall) {
				t.Fatalf("stale schema error masked the missing submission: %v", err)
			}
		})
	}
}

func TestFinalizationReminderRequiresTokenAdmission(t *testing.T) {
	calls := 0
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		calls++
		response := textResponse("no submission")
		response.Usage = fantasy.Usage{InputTokens: 1000}
		return response, nil
	})
	c.cfg.MaxAttempts = 3
	c.cfg.Ledger = NewLedger(1500)
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
	if !errors.Is(err, ErrBudgetExhausted) || !errors.Is(err, ErrNoFinalCall) || calls != 1 || out.Answer != "" {
		t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
	}
}

func TestFinalizationReminderPreservesCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	calls := 0
	c := testClient(func(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls == 1 {
			return textResponse("no submission"), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c.cfg.MaxAttempts = 3
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.CompleteStructured(ctx, "system", "user", "submit", &out)
	if !errors.Is(err, context.DeadlineExceeded) || calls != 2 || out.Answer != "" {
		t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
	}
}

func TestFinalizationFrameworkValidationFailureIsBounded(t *testing.T) {
	calls := 0
	c := testClient(func(context.Context, fantasy.Call) (*fantasy.Response, error) {
		calls++
		if calls > 2 {
			t.Fatal("framework validation failure caused unbounded finalization")
		}
		// This unmarshals into Go's zero value but lacks the schema's required
		// answer property. Fantasy must reject it without executing the tool.
		return toolCallResponse("submit", `{}`), nil
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.CompleteStructured(t.Context(), "system", "user", "submit", &out)
	if !errors.Is(err, ErrNoFinalCall) || calls != 2 || out.Answer != "" {
		t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
	}
}

func TestExplorationSchemaRejectionPreservesFinalizationAllowance(t *testing.T) {
	calls := 0
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		switch calls {
		case 1:
			// Go can unmarshal this, but Fantasy rejects the missing answer.
			return toolCallResponse("submit", `{}`), nil
		case 2:
			assertAutomaticFinalCall(t, call)
			return textResponse("not submitted yet"), nil
		default:
			assertAutomaticFinalCall(t, call)
			return toolCallResponse("submit", `{"answer":"done"}`), nil
		}
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 2, FinalTool: "submit", Tools: []Tool{
			{Name: "read", Schema: map[string]any{"type": "object"}, Run: func(context.Context, map[string]any) (any, error) {
				t.Error("exploration tool unexpectedly dispatched")
				return nil, nil
			}},
			{Name: "submit", Schema: reflectSchema(&out)},
		},
	}, &out)
	if err != nil || calls != 3 || out.Answer != "done" {
		t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
	}
}

func TestAgentFinalizationRetriesInvalidSubmission(t *testing.T) {
	calls := 0
	c := testClient(func(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
		calls++
		assertAutomaticFinalCall(t, call)
		if calls == 1 {
			return toolCallResponse("submit", `{"answer":42}`), nil
		}
		return toolCallResponse("submit", `{"answer":"corrected"}`), nil
	})
	c.cfg.MaxAttempts = 2
	var out struct {
		Answer string `json:"answer"`
	}
	err := c.Converse(t.Context(), "system", "user", ConverseOpts{
		MaxTurns: 1, FinalTool: "submit", Tools: basicTools(nil),
	}, &out)
	if err != nil || calls != 2 || out.Answer != "corrected" {
		t.Fatalf("error=%v calls=%d answer=%q", err, calls, out.Answer)
	}
}
