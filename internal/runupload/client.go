// Package runupload uploads immutable Roast run bundles directly to an
// operator-configured S3-compatible object store.
package runupload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/bluesky-social/roast/internal/config"
)

type uploadResult struct {
	Key       string
	ETag      string
	VersionID string
}

type objectStore interface {
	Destination() string
	Put(context.Context, string, io.Reader, int64, string) (uploadResult, error)
}

type s3Store struct {
	client      *minio.Client
	bucket      string
	prefix      string
	destination string
}

// NewStore constructs a direct S3-compatible uploader using only the
// explicitly resolved access key, secret, and optional session token.
func NewStore(cfg config.ResolvedUpload) (objectStore, error) {
	if cfg.Backend == "presigned" {
		return newPresignedStore(cfg)
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("parse upload endpoint %q", cfg.Endpoint)
	}
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken),
		Secure: u.Scheme == "https", Region: cfg.Region, BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize S3-compatible client: %w", err)
	}
	destination := strings.TrimSuffix(cfg.Endpoint, "/") + "/" + cfg.Bucket + "/" + strings.Trim(cfg.Prefix, "/")
	return &s3Store{client: client, bucket: cfg.Bucket, prefix: strings.Trim(cfg.Prefix, "/"), destination: destination}, nil
}

func (s *s3Store) Destination() string { return s.destination }

func (s *s3Store) Put(ctx context.Context, _ string, body io.Reader, size int64, digest string) (uploadResult, error) {
	key := path.Join(s.prefix, "sha256", digest+".tar.zst")
	info, err := s.client.PutObject(ctx, s.bucket, key, body, size, minio.PutObjectOptions{
		ContentType: "application/zstd", SendContentMd5: true, DisableContentSha256: true,
		UserMetadata: map[string]string{"roast-bundle-sha256": digest},
	})
	if err != nil {
		return uploadResult{}, err
	}
	return uploadResult{Key: info.Key, ETag: info.ETag, VersionID: info.VersionID}, nil
}

type presignedStore struct {
	endpoint      string
	token         string
	allowInsecure bool
	control       *http.Client
	data          *http.Client
}

type uploadGrant struct {
	RunID           string            `json:"run_id"`
	AttemptID       string            `json:"attempt_id"`
	ObjectKey       string            `json:"object_key"`
	Method          string            `json:"method"`
	UploadURL       string            `json:"upload_url"`
	RequiredHeaders map[string]string `json:"required_headers"`
	ExpiresAt       time.Time         `json:"expires_at"`
}

func newPresignedStore(cfg config.ResolvedUpload) (objectStore, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("parse presigned upload endpoint %q", cfg.Endpoint)
	}
	noRedirect := func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &presignedStore{
		endpoint: strings.TrimSuffix(cfg.Endpoint, "/"), token: cfg.Token, allowInsecure: cfg.AllowInsecureHTTP,
		control: &http.Client{CheckRedirect: noRedirect}, data: &http.Client{CheckRedirect: noRedirect},
	}, nil
}

// checkUploadURL requires a broker-issued upload URL to use HTTPS unless it
// is on loopback or the operator allowed plaintext HTTP for the broker.
func (s *presignedStore) checkUploadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("request presigned upload: broker returned an invalid upload URL")
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && (config.IsLoopbackHost(u.Hostname()) || s.allowInsecure):
		return nil
	case u.Scheme == "http":
		return fmt.Errorf("request presigned upload: broker returned a plaintext upload URL for %s; upload URLs must use https outside loopback unless upload.allow_insecure_http is set", u.Host)
	default:
		return fmt.Errorf("request presigned upload: broker returned an upload URL with unsupported scheme %q", u.Scheme)
	}
}

// redactURLError removes the query string and fragment, which carry the
// presigned signature, from a URL embedded in err.
func redactURLError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	if u, parseErr := url.Parse(urlErr.URL); parseErr == nil {
		u.RawQuery, u.Fragment, u.User = "", "", nil
		urlErr.URL = u.String()
	} else {
		urlErr.URL = "<upload URL>"
	}
	return err
}

func (s *presignedStore) Destination() string { return s.endpoint }

func (s *presignedStore) Put(ctx context.Context, runID string, body io.Reader, size int64, digest string) (uploadResult, error) {
	grant, err := s.create(ctx, runID, size, digest)
	if err != nil {
		return uploadResult{}, err
	}
	// Put accepts a borrowed Reader; the caller owns its lifetime. Without the
	// wrapper, net/http takes ownership when body is also an io.ReadCloser
	// (notably the run bundle's *os.File), breaking the caller's final Close.
	req, err := http.NewRequestWithContext(ctx, grant.Method, grant.UploadURL, io.NopCloser(body))
	if err != nil {
		return uploadResult{}, fmt.Errorf("build presigned upload request: %w", redactURLError(err))
	}
	req.ContentLength = size
	for name, value := range grant.RequiredHeaders {
		req.Header.Set(name, value)
	}
	resp, err := s.data.Do(req)
	if err != nil {
		return uploadResult{}, fmt.Errorf("put presigned bundle: %w", redactURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return uploadResult{}, responseError("put presigned bundle", resp)
	}
	versionID := resp.Header.Get("x-amz-version-id")
	if err := s.complete(ctx, runID, grant.AttemptID, versionID); err != nil {
		return uploadResult{}, err
	}
	return uploadResult{Key: grant.ObjectKey, ETag: strings.Trim(resp.Header.Get("ETag"), `"`), VersionID: versionID}, nil
}

func (s *presignedStore) create(ctx context.Context, runID string, size int64, digest string) (uploadGrant, error) {
	body := strings.NewReader(fmt.Sprintf(`{"run_id":%q,"bundle_sha256":%q,"bundle_bytes":%d}`, runID, digest, size))
	var grant uploadGrant
	if err := s.controlJSON(ctx, http.MethodPost, s.endpoint+"/api/v1/roast/runs", body, &grant); err != nil {
		return uploadGrant{}, fmt.Errorf("request presigned upload: %w", err)
	}
	if grant.AttemptID == "" || grant.ObjectKey == "" || grant.UploadURL == "" || !strings.EqualFold(grant.Method, http.MethodPut) {
		return uploadGrant{}, errors.New("request presigned upload: broker returned an incomplete grant")
	}
	if err := s.checkUploadURL(grant.UploadURL); err != nil {
		return uploadGrant{}, err
	}
	return grant, nil
}

func (s *presignedStore) complete(ctx context.Context, runID, attemptID, versionID string) error {
	body := strings.NewReader(fmt.Sprintf(`{"attempt_id":%q,"version_id":%q}`, attemptID, versionID))
	if err := s.controlJSON(ctx, http.MethodPost, s.endpoint+"/api/v1/roast/runs/"+url.PathEscape(runID)+"/complete", body, nil); err != nil {
		return fmt.Errorf("complete presigned upload: %w", err)
	}
	return nil
}

func (s *presignedStore) controlJSON(ctx context.Context, method, target string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.control.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError("broker request", resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode broker response: %w", err)
	}
	return nil
}

func responseError(operation string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	detail := strings.TrimSpace(string(raw))
	if len(detail) > 300 {
		detail = detail[:300] + "..."
	}
	return fmt.Errorf("%s: http %d: %s", operation, resp.StatusCode, detail)
}
