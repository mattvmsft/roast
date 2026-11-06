// Package updatecheck performs a cached, informational GitHub release check.
package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	LatestReleaseURL = "https://api.github.com/repos/bluesky-social/roast/releases/latest"
	defaultMaxAge    = 24 * time.Hour
	maxResponseBytes = 1 << 20
)

// Checker contains the replaceable dependencies used by a release check.
type Checker struct {
	LatestURL string
	CachePath string
	Client    *http.Client
	Now       func() time.Time
	MaxAge    time.Duration
}

// Result describes the latest release known to the checker. Error is advisory:
// release-check failures never prevent Roast from running.
type Result struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	Source    string    `json:"source,omitempty"`
	Error     string    `json:"error,omitempty"`
	Skipped   bool      `json:"skipped,omitempty"`
}

// UpdateAvailable reports whether Latest is a stable semantic version newer
// than Current.
func (r Result) UpdateAvailable() bool {
	return Newer(r.Latest, r.Current)
}

type cacheFile struct {
	Latest    string    `json:"latest,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
}

// Default returns the production checker.
func Default() (Checker, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return Checker{}, fmt.Errorf("resolve user cache directory: %w", err)
	}
	return Checker{
		LatestURL: LatestReleaseURL,
		CachePath: filepath.Join(dir, "roast", "release-check.json"),
		Client:    &http.Client{Timeout: 5 * time.Second},
		Now:       time.Now,
		MaxAge:    defaultMaxAge,
	}, nil
}

func (c Checker) defaults() Checker {
	if c.LatestURL == "" {
		c.LatestURL = LatestReleaseURL
	}
	if c.Client == nil {
		c.Client = &http.Client{Timeout: 5 * time.Second}
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.MaxAge <= 0 {
		c.MaxAge = defaultMaxAge
	}
	return c
}

// Cached reads the last release-check result without network access. A missing
// cache is returned as an empty result, not an error.
func (c Checker) Cached(current string) (Result, error) {
	c = c.defaults()
	result := Result{Current: current, Source: "cache"}
	if !checkable(current) {
		result.Skipped = true
		return result, nil
	}
	data, err := os.ReadFile(c.CachePath)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("read update cache: %w", err)
	}
	var cached cacheFile
	if err := json.Unmarshal(data, &cached); err != nil {
		return result, fmt.Errorf("parse update cache: %w", err)
	}
	result.Latest = cached.Latest
	result.CheckedAt = cached.CheckedAt
	result.Error = cached.Error
	return result, nil
}

// NeedsRefresh reports whether a cached result is absent or old enough to
// refresh. Development and malformed versions are never checked.
func (c Checker) NeedsRefresh(result Result) bool {
	c = c.defaults()
	if result.Skipped || !checkable(result.Current) {
		return false
	}
	return result.CheckedAt.IsZero() || c.Now().Sub(result.CheckedAt) >= c.MaxAge
}

// Check returns a fresh GitHub release result. If force is false, a fresh
// cache is returned without network access. Network errors are persisted only
// to rate-limit retries; a previously known latest version is retained.
func (c Checker) Check(ctx context.Context, current string, force bool) Result {
	c = c.defaults()
	cached, cacheErr := c.Cached(current)
	if cached.Skipped {
		return cached
	}
	if cacheErr == nil && !force && !c.NeedsRefresh(cached) {
		return cached
	}

	result := cached
	result.Current = current
	result.Source = "network"
	result.CheckedAt = c.Now().UTC()
	latest, err := c.fetch(ctx, current)
	if err != nil {
		result.Error = err.Error()
	} else {
		result.Latest = latest
		result.Error = ""
	}
	if writeErr := c.writeCache(cacheFile{Latest: result.Latest, CheckedAt: result.CheckedAt, Error: result.Error}); writeErr != nil && result.Error == "" {
		result.Error = writeErr.Error()
	}
	return result
}

func (c Checker) fetch(ctx context.Context, current string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.LatestURL, nil)
	if err != nil {
		return "", fmt.Errorf("create release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "roast/"+current)
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("check latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("check latest release: HTTP %s", resp.Status)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
	if err := decoder.Decode(&body); err != nil {
		return "", fmt.Errorf("decode latest release: %w", err)
	}
	version, ok := parseVersion(body.TagName)
	if !ok || version.pre != "" {
		return "", fmt.Errorf("latest release has invalid stable version %q", body.TagName)
	}
	return body.TagName, nil
}

func (c Checker) writeCache(cached cacheFile) error {
	data, err := json.Marshal(cached)
	if err != nil {
		return fmt.Errorf("encode update cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.CachePath), 0o700); err != nil {
		return fmt.Errorf("create update cache directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(c.CachePath), ".release-check-*.tmp")
	if err != nil {
		return fmt.Errorf("create update cache temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("set update cache permissions: %w", err)
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write update cache: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close update cache: %w", err)
	}
	if err := os.Rename(temporaryPath, c.CachePath); err != nil {
		return fmt.Errorf("replace update cache: %w", err)
	}
	keep = true
	return nil
}

func checkable(raw string) bool {
	_, ok := parseVersion(raw)
	return raw != "dev" && ok
}

type version struct {
	major, minor, patch int
	pre                 string
}

func parseVersion(raw string) (version, bool) {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "v"))
	if build := strings.IndexByte(raw, '+'); build >= 0 {
		raw = raw[:build]
	}
	var pre string
	if dash := strings.IndexByte(raw, '-'); dash >= 0 {
		pre = raw[dash+1:]
		raw = raw[:dash]
		if pre == "" {
			return version{}, false
		}
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	numbers := make([]int, 3)
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return version{}, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return version{}, false
		}
		numbers[i] = n
	}
	return version{major: numbers[0], minor: numbers[1], patch: numbers[2], pre: pre}, true
}

// Newer compares semantic release versions. Invalid or development versions
// are never considered updateable.
func Newer(latest, current string) bool {
	l, lok := parseVersion(latest)
	c, cok := parseVersion(current)
	if !lok || !cok || l.pre != "" {
		return false
	}
	for _, pair := range [][2]int{{l.major, c.major}, {l.minor, c.minor}, {l.patch, c.patch}} {
		if pair[0] != pair[1] {
			return pair[0] > pair[1]
		}
	}
	return c.pre != ""
}
