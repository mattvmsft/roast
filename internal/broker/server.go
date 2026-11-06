package broker

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const maxRequestBytes = 1 << 20

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Server struct {
	Store           ObjectStore
	SigningKey      []byte
	PrincipalHeader string
	Prefix          string
	MaxBundleBytes  int64
	GrantTTL        time.Duration
	CompletionTTL   time.Duration
	Logger          *slog.Logger
	Now             func() time.Time
	ClientConfig    []byte
}

type createRequest struct {
	RunID        string `json:"run_id"`
	BundleSHA256 string `json:"bundle_sha256"`
	BundleBytes  int64  `json:"bundle_bytes"`
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

type completeRequest struct {
	AttemptID string `json:"attempt_id"`
	VersionID string `json:"version_id"`
}

type completionToken struct {
	Principal string `json:"principal"`
	RunID     string `json:"run_id"`
	ObjectKey string `json:"object_key"`
	Digest    string `json:"digest"`
	Bytes     int64  `json:"bytes"`
	Expires   int64  `json:"expires"`
}

func (s *Server) Handler() (http.Handler, error) {
	if s.Store == nil {
		return nil, errors.New("broker object store is required")
	}
	if len(s.SigningKey) < 32 {
		return nil, errors.New("broker signing key must be at least 32 bytes")
	}
	if strings.TrimSpace(s.PrincipalHeader) == "" {
		return nil, errors.New("broker principal header is required")
	}
	if s.MaxBundleBytes <= 0 {
		s.MaxBundleBytes = 128 << 20
	}
	if s.GrantTTL <= 0 {
		s.GrantTTL = 5 * time.Minute
	}
	if s.CompletionTTL <= 0 {
		s.CompletionTTL = 24 * time.Hour
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	s.Prefix = strings.Trim(strings.TrimSpace(s.Prefix), "/")
	if s.Prefix == "" {
		s.Prefix = "roast-runs"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	if len(s.ClientConfig) > 0 {
		mux.HandleFunc("GET /config/v1", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/yaml")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(s.ClientConfig)
		})
	}
	mux.HandleFunc("POST /api/v1/roast/runs", s.create)
	mux.HandleFunc("POST /api/v1/roast/runs/{run_id}/complete", s.complete)
	return mux, nil
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req createRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.RunID = strings.TrimSpace(req.RunID)
	req.BundleSHA256 = strings.TrimSpace(req.BundleSHA256)
	if !runIDPattern.MatchString(req.RunID) {
		writeError(w, http.StatusBadRequest, "run_id must be 1-128 letters, digits, '.', '_' or '-'")
		return
	}
	if _, err := digestBase64(req.BundleSHA256); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.BundleBytes <= 0 || req.BundleBytes > s.MaxBundleBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("bundle_bytes must be between 1 and %d", s.MaxBundleBytes))
		return
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		s.fail(w, "generate upload id", err)
		return
	}
	principalHash := sha256.Sum256([]byte(strings.ToLower(principal)))
	key := strings.Join([]string{s.Prefix, hex.EncodeToString(principalHash[:16]), req.RunID, hex.EncodeToString(nonce), "bundle.tar.zst"}, "/")
	expires := s.Now().UTC().Add(s.GrantTTL)
	put, err := s.Store.PresignPut(r.Context(), key, req.BundleBytes, req.BundleSHA256, s.GrantTTL)
	if err != nil {
		s.fail(w, "presign upload", err)
		return
	}
	token, err := s.sign(completionToken{
		Principal: principal, RunID: req.RunID, ObjectKey: key,
		Digest: req.BundleSHA256, Bytes: req.BundleBytes, Expires: expires.Add(s.CompletionTTL).Unix(),
	})
	if err != nil {
		s.fail(w, "sign completion token", err)
		return
	}
	writeJSON(w, http.StatusOK, uploadGrant{
		RunID: req.RunID, AttemptID: token, ObjectKey: key, Method: put.Method,
		UploadURL: put.URL, RequiredHeaders: put.Headers, ExpiresAt: expires,
	})
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	runID := r.PathValue("run_id")
	var req completeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	grant, err := s.verify(req.AttemptID)
	if err != nil || grant.Principal != principal || grant.RunID != runID {
		writeError(w, http.StatusNotFound, "upload attempt not found")
		return
	}
	if s.Now().UTC().Unix() > grant.Expires {
		writeError(w, http.StatusConflict, "upload attempt expired; request a new upload")
		return
	}
	info, err := s.Store.Stat(r.Context(), grant.ObjectKey, req.VersionID)
	if errors.Is(err, ErrObjectNotFound) {
		writeError(w, http.StatusBadRequest, "uploaded object not found")
		return
	}
	if err != nil {
		s.fail(w, "verify uploaded object", err)
		return
	}
	wantChecksum, _ := digestBase64(grant.Digest)
	if info.Size != grant.Bytes || info.SHA256Base64 == "" || info.SHA256Base64 != wantChecksum {
		writeError(w, http.StatusBadRequest, "uploaded object does not match its declared size and sha256")
		return
	}
	if req.VersionID != "" && info.VersionID != "" && req.VersionID != info.VersionID {
		writeError(w, http.StatusBadRequest, "uploaded object version does not match")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id": grant.RunID, "status": "uploaded", "bundle_key": grant.ObjectKey,
		"bundle_version_id": info.VersionID, "bundle_bytes": grant.Bytes, "bundle_digest": grant.Digest,
	})
}

func (s *Server) sign(token completionToken) (string, error) {
	payload, err := json.Marshal(token)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.SigningKey)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Server) verify(raw string) (completionToken, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return completionToken{}, errors.New("invalid token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return completionToken{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return completionToken{}, err
	}
	mac := hmac.New(sha256.New, s.SigningKey)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return completionToken{}, errors.New("invalid signature")
	}
	var token completionToken
	if err := json.Unmarshal(payload, &token); err != nil {
		return completionToken{}, err
	}
	return token, nil
}

func (s *Server) principal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principal := strings.TrimSpace(r.Header.Get(s.PrincipalHeader))
	if principal == "" {
		writeError(w, http.StatusUnauthorized, "authenticated principal unavailable")
		return "", false
	}
	return principal, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request must contain one JSON object")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) fail(w http.ResponseWriter, operation string, err error) {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error(operation, "error", err)
	writeError(w, http.StatusServiceUnavailable, operation+" failed")
}

func Shutdown(ctx context.Context, server *http.Server) error {
	return server.Shutdown(ctx)
}
