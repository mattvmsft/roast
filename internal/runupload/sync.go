package runupload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/runbundle"
)

type ResultKind string

const (
	ResultUploaded        ResultKind = "uploaded"
	ResultAlreadyDone     ResultKind = "already_done"
	ResultTransientFailed ResultKind = "transient_failed"
	ResultSkipped         ResultKind = "skipped"
	syncConcurrency                  = 4
)

type Outcome struct {
	RunID  string
	Result ResultKind
	Detail string
}

func uploadRun(ctx context.Context, store objectStore, recordDir, runID string, includeExisting bool) Outcome {
	state, err := readState(recordDir, runID)
	if err != nil {
		return Outcome{RunID: runID, Result: ResultSkipped, Detail: "corrupt local upload state: " + err.Error()}
	}
	if state != nil {
		if state.Status == StatusUploaded && state.Destination == store.Destination() {
			return Outcome{RunID: runID, Result: ResultAlreadyDone, Detail: state.ObjectKey}
		}
		if state.Status == StatusSkippedPreEnrollment && !includeExisting {
			return Outcome{RunID: runID, Result: ResultSkipped, Detail: "predates upload opt-in (pass --include-existing to backfill)"}
		}
	}

	runDir := filepath.Join(recordDir, "runs", runID)
	if _, err := os.Stat(filepath.Join(runDir, record.MetaFile)); err != nil {
		return Outcome{RunID: runID, Result: ResultSkipped, Detail: "run has no complete metadata"}
	}
	bundlePath, digest, size, err := runbundle.Build(runDir)
	if err != nil {
		return pendingOutcome(recordDir, runID, state, store.Destination(), fmt.Errorf("build bundle: %w", err))
	}
	bundle, err := os.Open(bundlePath)
	if err != nil {
		return pendingOutcome(recordDir, runID, state, store.Destination(), fmt.Errorf("open bundle: %w", err))
	}
	result, uploadErr := store.Put(ctx, runID, bundle, size, digest)
	closeErr := bundle.Close()
	if uploadErr != nil {
		return pendingOutcome(recordDir, runID, state, store.Destination(), fmt.Errorf("upload bundle: %w", uploadErr))
	}
	if closeErr != nil {
		return pendingOutcome(recordDir, runID, state, store.Destination(), fmt.Errorf("close bundle: %w", closeErr))
	}

	attempts := 1
	if state != nil {
		attempts = state.Attempts + 1
	}
	done := &State{
		Status: StatusUploaded, Destination: store.Destination(),
		ObjectKey: result.Key, ETag: result.ETag, VersionID: result.VersionID,
		Digest: digest, Bytes: size, Attempts: attempts, LastAttempt: time.Now().UTC(),
	}
	if err := writeState(recordDir, runID, done); err != nil {
		return Outcome{RunID: runID, Result: ResultTransientFailed, Detail: "uploaded, but could not persist local state: " + err.Error()}
	}
	return Outcome{RunID: runID, Result: ResultUploaded, Detail: result.Key}
}

func pendingOutcome(recordDir, runID string, previous *State, destination string, cause error) Outcome {
	attempts := 1
	if previous != nil {
		attempts = previous.Attempts + 1
	}
	state := &State{Status: StatusPending, Destination: destination, Attempts: attempts, LastAttempt: time.Now().UTC(), LastError: cause.Error()}
	if err := writeState(recordDir, runID, state); err != nil {
		return Outcome{RunID: runID, Result: ResultTransientFailed, Detail: cause.Error() + "; persist upload state: " + err.Error()}
	}
	return Outcome{RunID: runID, Result: ResultTransientFailed, Detail: cause.Error()}
}

func discoverCandidates(recordDir, destination string, includeExisting bool) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(recordDir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		if _, err := os.Stat(filepath.Join(recordDir, "runs", runID, record.MetaFile)); err != nil {
			continue
		}
		state, err := readState(recordDir, runID)
		if err != nil {
			continue
		}
		if state == nil || state.Status == StatusPending || (state.Status == StatusUploaded && state.Destination != destination) || (includeExisting && state.Status == StatusSkippedPreEnrollment) {
			candidates = append(candidates, runID)
		}
	}
	sort.Strings(candidates)
	return candidates, nil
}

// Sync uploads every eligible local run with bounded concurrency. Upload
// errors are recorded per run and do not stop unrelated bundles.
func Sync(ctx context.Context, store objectStore, recordDir string, includeExisting bool) ([]Outcome, error) {
	if err := EnsureEnrolled(recordDir); err != nil {
		return nil, fmt.Errorf("establish upload enrollment: %w", err)
	}
	candidates, err := discoverCandidates(recordDir, store.Destination(), includeExisting)
	if err != nil {
		return nil, err
	}

	sem := make(chan struct{}, syncConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	outcomes := make([]Outcome, 0, len(candidates))
	for _, runID := range candidates {
		runID := runID
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			outcome := uploadRun(ctx, store, recordDir, runID, includeExisting)
			mu.Lock()
			outcomes = append(outcomes, outcome)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].RunID < outcomes[j].RunID })
	if err := ctx.Err(); err != nil {
		return outcomes, err
	}
	return outcomes, nil
}
