// Package llm adapts Fantasy inference models to Roast's finder and verifier.
package llm

import (
	"context"
	"fmt"
	"time"
)

// Usage accumulates token counts across a client's calls.
//
// Fantasy's common usage shape is the accounting input. Roast normalizes each
// protocol's cache semantics into disjoint ordinary input, cache-read, and
// cache-write counts exactly once when each response arrives.
type Usage struct {
	UncachedInputTokens int `json:"uncached_input_tokens"`
	CachedInputTokens   int `json:"cached_input_tokens"`
	CacheWriteTokens    int `json:"cache_write_tokens"`
	OutputTokens        int `json:"output_tokens"`
	Calls               int `json:"calls"`
}

// ToolFunc executes one model-requested tool call and returns a
// JSON-serializable result. Implementations must treat args as untrusted.
type ToolFunc func(ctx context.Context, args map[string]any) (any, error)

// Tool is one function exposed to the model in a Converse loop.
type Tool struct {
	Name        string
	Description string
	// Schema is a JSON Schema object for the arguments.
	Schema map[string]any
	// Run is nil for the final tool (whose arguments are returned to the
	// caller instead of dispatched).
	Run ToolFunc
}

// TurnInfo surfaces one tool-call round-trip for progress logging.
type TurnInfo struct {
	Turn int
	Tool string
	Args map[string]any
}

// ConverseOpts configures a multi-turn tool loop.
type ConverseOpts struct {
	Tools []Tool
	// FinalTool names the tool whose arguments end the loop and become
	// the return value.
	FinalTool string
	MaxTurns  int
	// OnTurn, when non-nil, is called after each dispatched tool call.
	OnTurn func(TurnInfo)
	// InputByteCeiling bounds conversation growth: once exceeded, the
	// model is told to wrap up rather than sending an oversized request.
	// Zero means the implementation default.
	InputByteCeiling int
	// WorkDeadline, when non-zero, is the run's work deadline: once it passes,
	// no further exploration turns run — the loop jumps straight to the
	// reserved wrap-up turn (final tool only, forced-submit notice) executed
	// under its own WrapUpWindow timeout derived fresh from the caller's
	// context. The deadline is checked against the clock, deliberately NOT
	// implemented as a context.WithTimeout around the whole run — that would
	// cancel the wrap-up turn along with the work.
	WorkDeadline time.Time
	// WrapUpWindow bounds the wrap-up call after WorkDeadline fires.
	// Zero means 90s.
	WrapUpWindow time.Duration
	// OnWorkDeadline, when non-nil, is called once if WorkDeadline fires —
	// the caller's signal that exploration was cut short even when the
	// wrap-up submission succeeds (survey completion must read partial).
	OnWorkDeadline func()
	// OnBudgetExhausted, when non-nil, is called once if the token ledger
	// refuses an exploration call — the budget counterpart of
	// OnWorkDeadline, for the same reason.
	OnBudgetExhausted func()
}

// workDeadlineState tracks work-deadline mechanics around the Fantasy agent:
// a latched clock check (never a whole-run context timeout) plus a bounded
// child context for the wrap-up turn.
type workDeadlineState struct {
	deadline time.Time
	window   time.Duration
	onFire   func()
	fired    bool
}

func newWorkDeadlineState(opts ConverseOpts) *workDeadlineState {
	w := &workDeadlineState{
		deadline: opts.WorkDeadline,
		window:   opts.WrapUpWindow,
		onFire:   opts.OnWorkDeadline,
	}
	if w.window <= 0 {
		w.window = 90 * time.Second
	}
	return w
}

// check reports whether the work deadline has fired, latching the first
// firing and invoking onFire exactly once.
func (w *workDeadlineState) check() bool {
	if w.fired {
		return true
	}
	if w.deadline.IsZero() || time.Now().Before(w.deadline) {
		return false
	}
	w.fired = true
	if w.onFire != nil {
		w.onFire()
	}
	return true
}

// callCtx returns the context for this turn's model call. Before the
// deadline fires, exploration calls are bounded BY the deadline so an
// in-flight call cannot sail past it (the loop catches the resulting
// DeadlineExceeded, latches, and proceeds to the wrap-up turn). After it
// fires, the wrap-up turn gets its own bounded child so a stuck final
// submit cannot hang the run past the reserved window.
func (w *workDeadlineState) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if w.fired {
		return context.WithTimeout(ctx, w.window)
	}
	if !w.deadline.IsZero() {
		return context.WithDeadline(ctx, w.deadline)
	}
	return ctx, func() {}
}

// runTool dispatches one exploration tool call, bounding the WAIT rather
// than the tool: Run executes in a goroutine and the wait selects on the
// deadline-bounded context, so a context-ignoring tool (the local helpers
// take no ctx; a hung git subprocess under exec.Command) cannot stall the
// loop past the work deadline — at worst its goroutine is abandoned to
// finish in the background. A dispatch arriving after the deadline latched
// (later calls in a multi-tool response) short-circuits without starting
// new exploration. Panics are contained here since Run leaves the caller's
// stack.
func (w *workDeadlineState) runTool(ctx context.Context, tool *Tool, args map[string]any) (any, error) {
	if w.check() {
		return nil, ErrWorkDeadline
	}
	toolCtx, cancel := w.callCtx(ctx)
	defer cancel()
	type res struct {
		v   any
		err error
	}
	ch := make(chan res, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- res{err: fmt.Errorf("tool panicked: %v", r)}
			}
		}()
		v, err := tool.Run(toolCtx, args)
		ch <- res{v, err}
	}()
	select {
	case r := <-ch:
		w.check() // the deadline may have crossed during the tool: latch now
		return r.v, r.err
	case <-toolCtx.Done():
		w.check()
		return nil, toolCtx.Err()
	}
}

// wrapUpNotice is the forced-submit message for the reserved last turn.
func (w *workDeadlineState) wrapUpNotice(finalTool string) string {
	if w.fired {
		return "WORK DEADLINE REACHED — call `" + finalTool + "` NOW with everything you have; partial results beat none."
	}
	return "Exploration budget exhausted — call `" + finalTool + "` NOW with everything you have; partial results beat none."
}

// Client is one configured model role (finder or verifier).
type Client interface {
	// Name is the request body's model field.
	Name() string

	// CompleteStructured runs one completion constrained to schema and
	// unmarshals the response into out (a pointer to a struct whose
	// schema was reflected). Retries once with the validation error fed
	// back when the response fails to parse.
	CompleteStructured(ctx context.Context, system, user string, schemaName string, out any) error

	// Converse runs a multi-turn tool loop until the model calls
	// opts.FinalTool, whose arguments are unmarshaled into out.
	// Returns ErrNoFinalCall if MaxTurns elapses first.
	Converse(ctx context.Context, system, user string, opts ConverseOpts, out any) error

	// Usage reports accumulated token counts.
	Usage() Usage
}
