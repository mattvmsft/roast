package llm

import (
	"errors"
	"sync"
)

// ErrBudgetExhausted means the run's token-reservation ledger refused a
// call. The Converse loops convert it into a forced wrap-up (the final
// submit draws on a standing allowance); the verify stage stops launching
// verifications and remaining findings gate unverified.
var ErrBudgetExhausted = errors.New("llm: run token budget exhausted")

const (
	// estBytesPerToken is the conservative admission-side tokenization:
	// code-heavy prompts run ~3 bytes/token, so dividing by 3 over-estimates
	// tokens for typical English/code mixes — the safe direction for
	// admission control.
	estBytesPerToken = 3
	// defaultFinalAllowance is the input-equivalent token slice reserved up
	// front for the final submit, so exhaustion can never leave a run unable
	// to flush what it found (no stop path may discard accumulated work).
	defaultFinalAllowance = 100_000
	// cachedTokenWeight discounts provider-cache reads at reconciliation
	// (cache reads bill ~0.1x on both providers).
	cachedTokenWeight = 0.1
)

// Ledger is the shared per-run token-reservation ledger: one instance
// spans the finder and verifier clients. Admission reserves a call's WHOLE
// footprint before it launches — a conservative tokenization of the full
// serialized request (which on chat replays the whole transcript every turn,
// so input is the large growing part) plus the output cap, all treated as
// uncached because cache-hit status is only known post-response. After the
// call, reconciliation replaces the reservation with the provider-reported
// usage, cache reads weighted at 0.1.
//
// A nil *Ledger admits everything (no budget configured).
type Ledger struct {
	mu             sync.Mutex
	limit          float64
	finalAllowance float64
	committed      float64
	outstanding    float64
	exhausted      bool
}

// NewLedger returns a ledger enforcing limit input-equivalent tokens, or nil
// (unlimited) when limit <= 0.
func NewLedger(limit int) *Ledger {
	if limit <= 0 {
		return nil
	}
	l := &Ledger{limit: float64(limit), finalAllowance: defaultFinalAllowance}
	if l.finalAllowance > l.limit/2 {
		l.finalAllowance = l.limit / 2
	}
	return l
}

// Reserve admits a call whose serialized request is reqBytes long with a
// maxOutput completion cap. final marks the reserved wrap-up submit, which
// may draw on the standing final allowance. On success the returned release
// MUST be called exactly once with the provider-reported usage (zero Usage
// for a failed call) to reconcile the reservation.
func (l *Ledger) Reserve(reqBytes, maxOutput int, final bool) (func(Usage), error) {
	if l == nil {
		return func(Usage) {}, nil
	}
	est := float64(reqBytes)/estBytesPerToken + float64(maxOutput)
	l.mu.Lock()
	defer l.mu.Unlock()
	headroom := l.limit
	if !final {
		// Hold back this request's own footprint PLUS a worst-case response:
		// the wrap-up call replays the same transcript grown by this call's
		// output, so a smaller hold-back could admit exploration the final
		// flush can no longer afford — violating the no-discard invariant.
		headroom -= max(l.finalAllowance, est+float64(maxOutput))
	}
	if l.committed+l.outstanding+est > headroom {
		l.exhausted = true
		return nil, ErrBudgetExhausted
	}
	l.outstanding += est
	var once sync.Once
	return func(u Usage) {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.outstanding -= est
			l.committed += weightedTokens(u)
		})
	}, nil
}

// Exhausted reports whether any reservation has been refused — the verify
// stage's signal to stop launching verifications.
func (l *Ledger) Exhausted() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exhausted
}

// Committed returns the reconciled input-equivalent tokens spent so far and
// the limit, for the run record's budget provenance.
func (l *Ledger) Committed() (spent, limit int) {
	if l == nil {
		return 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return int(l.committed), int(l.limit)
}

// weightedTokens is the reconciliation weight: uncached input, cache writes,
// and output at full weight; cache reads at cachedTokenWeight.
// UncachedInputTokens and CachedInputTokens are independent counts (see
// Usage's doc comment) — under the chat/Anthropic convention cached can
// legitimately exceed uncached, so there is no relationship between them to
// re-derive or clamp here. Every field is floored independently: the
// defensive floor guards only against a negative provider-reported count,
// never against the fields disagreeing with each other.
func weightedTokens(u Usage) float64 {
	uncached := max(u.UncachedInputTokens, 0)
	cached := max(u.CachedInputTokens, 0)
	cacheWrite := max(u.CacheWriteTokens, 0)
	output := max(u.OutputTokens, 0)
	return float64(uncached) +
		cachedTokenWeight*float64(cached) +
		float64(cacheWrite) +
		float64(output)
}
