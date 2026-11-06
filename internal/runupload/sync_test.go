package runupload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/roast/internal/config"
	"github.com/bluesky-social/roast/internal/record"
	"github.com/bluesky-social/roast/internal/runbundle"
)

type fakeStore struct {
	mu          sync.Mutex
	destination string
	err         error
	puts        int
	body        []byte
}

func (f *fakeStore) Destination() string { return f.destination }

func (f *fakeStore) Put(_ context.Context, _ string, body io.Reader, _ int64, digest string) (uploadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.err != nil {
		return uploadResult{}, f.err
	}
	f.body, _ = io.ReadAll(body)
	return uploadResult{Key: "runs/sha256/" + digest + ".tar.zst", ETag: "etag"}, nil
}

func makeRun(t *testing.T, recordDir, runID string) string {
	t.Helper()
	runDir := filepath.Join(recordDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, record.MetaFile), []byte(`{"run_id":"`+runID+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "result.json.gz"), []byte("result"), 0o644); err != nil {
		t.Fatal(err)
	}
	return runDir
}

func TestSyncRetriesPendingBundleAndKeepsStateOutsideBundle(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureEnrolled(dir); err != nil {
		t.Fatal(err)
	}
	runDir := makeRun(t, dir, "20260920T120000Z-1234abcd")
	failing := &fakeStore{destination: "https://objects.example/bucket/runs", err: errors.New("temporary outage")}
	outcomes, err := Sync(t.Context(), failing, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].Result != ResultTransientFailed {
		t.Fatalf("first outcomes = %#v", outcomes)
	}
	state, err := readState(dir, "20260920T120000Z-1234abcd")
	if err != nil || state == nil || state.Status != StatusPending || state.Attempts != 1 {
		t.Fatalf("pending state = %#v, err=%v", state, err)
	}

	succeeding := &fakeStore{destination: failing.destination}
	outcomes, err = Sync(t.Context(), succeeding, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].Result != ResultUploaded || succeeding.puts != 1 {
		t.Fatalf("retry outcomes = %#v, puts=%d", outcomes, succeeding.puts)
	}
	state, err = readState(dir, "20260920T120000Z-1234abcd")
	if err != nil || state.Status != StatusUploaded || state.Attempts != 2 || state.ObjectKey == "" {
		t.Fatalf("uploaded state = %#v, err=%v", state, err)
	}
	if _, err := runbundle.Verify(filepath.Join(runDir, runbundle.BundleFile)); err != nil {
		t.Fatalf("uploaded bundle no longer verifies: %v", err)
	}
	manifest, err := runbundle.Verify(filepath.Join(runDir, runbundle.BundleFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Files {
		if strings.Contains(entry.Path, "upload") {
			t.Fatalf("local upload state leaked into immutable bundle: %s", entry.Path)
		}
	}
	if len(succeeding.body) == 0 || bytes.Equal(succeeding.body, []byte("result")) {
		t.Fatal("store did not receive the compressed run bundle")
	}
}

func TestSyncReuploadsWhenDestinationChanges(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureEnrolled(dir); err != nil {
		t.Fatal(err)
	}
	makeRun(t, dir, "run-1")
	first := &fakeStore{destination: "https://one.example/bucket/runs"}
	if _, err := Sync(t.Context(), first, dir, false); err != nil {
		t.Fatal(err)
	}
	second := &fakeStore{destination: "https://two.example/bucket/runs"}
	outcomes, err := Sync(t.Context(), second, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].Result != ResultUploaded || second.puts != 1 {
		t.Fatalf("destination-change outcomes = %#v, puts=%d", outcomes, second.puts)
	}
}

func TestEnrollmentDoesNotBackfillOldRuns(t *testing.T) {
	dir := t.TempDir()
	makeRun(t, dir, "old-run")
	makeRun(t, dir, "current-run")
	if err := ensureEnrolledExcept(dir, "current-run"); err != nil {
		t.Fatal(err)
	}
	old, _ := readState(dir, "old-run")
	current, _ := readState(dir, "current-run")
	if old == nil || old.Status != StatusSkippedPreEnrollment || current != nil {
		t.Fatalf("old=%#v current=%#v", old, current)
	}
}

func TestAutoUploadConfigurationFailureLeavesPendingOutbox(t *testing.T) {
	dir := t.TempDir()
	makeRun(t, dir, "run-1")
	cfg := config.Config{Upload: config.Upload{
		Enabled: true, Endpoint: "https://objects.example", Bucket: "bucket", Prefix: "runs",
		AccessKeyEnv: "MISSING_UPLOAD_ACCESS", SecretKeyEnv: "MISSING_UPLOAD_SECRET",
	}}
	t.Setenv("MISSING_UPLOAD_ACCESS", "")
	t.Setenv("MISSING_UPLOAD_SECRET", "")
	MaybeAutoUpload(cfg, dir, "run-1")
	state, err := readState(dir, "run-1")
	if err != nil || state == nil || state.Status != StatusPending || !strings.Contains(state.LastError, "credentials") {
		t.Fatalf("automatic failure state = %#v, err=%v", state, err)
	}
}
