package updatecheck

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	tests := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.0", "v1.1.9", true},
		{"v1.2.0", "1.2.0", false},
		{"v1.2.0", "v1.2.0-rc.1", true},
		{"v1.2.0-rc.1", "v1.1.0", false},
		{"garbage", "v1.0.0", false},
		{"v1.2.0", "dev", false},
	}
	for _, test := range tests {
		if got := Newer(test.latest, test.current); got != test.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", test.latest, test.current, got, test.want)
		}
	}
}

func TestCheckCachesSuccessfulRelease(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("User-Agent"); got != "roast/v1.0.0" {
			t.Errorf("User-Agent = %q", got)
		}
		_, _ = w.Write([]byte(`{"tag_name":"v1.1.0"}`))
	}))
	defer server.Close()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	checker := Checker{
		LatestURL: server.URL,
		CachePath: filepath.Join(t.TempDir(), "release.json"),
		Client:    server.Client(),
		Now:       func() time.Time { return now },
		MaxAge:    time.Hour,
	}
	result := checker.Check(t.Context(), "v1.0.0", false)
	if result.Error != "" || result.Latest != "v1.1.0" || !result.UpdateAvailable() || result.Source != "network" {
		t.Fatalf("Check() = %#v", result)
	}
	result = checker.Check(t.Context(), "v1.0.0", false)
	if result.Source != "cache" || requests != 1 {
		t.Fatalf("cached Check() = %#v, requests=%d", result, requests)
	}
}

func TestCheckFailureRetainsCachedLatest(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	checker := Checker{
		LatestURL: server.URL,
		CachePath: filepath.Join(t.TempDir(), "release.json"),
		Client:    server.Client(),
		Now:       func() time.Time { return now },
	}
	if err := checker.writeCache(cacheFile{Latest: "v1.1.0", CheckedAt: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	result := checker.Check(t.Context(), "v1.0.0", true)
	if result.Error == "" || result.Latest != "v1.1.0" || !result.UpdateAvailable() {
		t.Fatalf("Check() = %#v", result)
	}
}

func TestDevelopmentBuildSkipsCheck(t *testing.T) {
	checker := Checker{CachePath: filepath.Join(t.TempDir(), "release.json")}
	result := checker.Check(t.Context(), "dev", true)
	if !result.Skipped || result.Error != "" {
		t.Fatalf("Check() = %#v", result)
	}
}

func TestConcurrentCacheWritesRemainValid(t *testing.T) {
	checker := Checker{CachePath: filepath.Join(t.TempDir(), "release.json")}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := checker.writeCache(cacheFile{Latest: "v1.2.3", CheckedAt: time.Now()}); err != nil {
				t.Errorf("writeCache() error = %v", err)
			}
		}()
	}
	group.Wait()
	data, err := os.ReadFile(checker.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	var cached cacheFile
	if err := json.Unmarshal(data, &cached); err != nil || cached.Latest != "v1.2.3" {
		t.Fatalf("cache = %q, decode error = %v", data, err)
	}
}
