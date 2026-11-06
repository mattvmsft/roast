package broker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeStore struct {
	put  PresignedPut
	info ObjectInfo
	key  string
}

func (f *fakeStore) PresignPut(_ context.Context, key string, _ int64, _ string, _ time.Duration) (PresignedPut, error) {
	f.key = key
	return f.put, nil
}

func (f *fakeStore) Stat(_ context.Context, key, _ string) (ObjectInfo, error) {
	if key != f.key {
		return ObjectInfo{}, ErrObjectNotFound
	}
	return f.info, nil
}

func TestBrokerCreateAndComplete(t *testing.T) {
	digestRaw := bytes.Repeat([]byte{0x42}, 32)
	digest := hex.EncodeToString(digestRaw)
	store := &fakeStore{
		put:  PresignedPut{URL: "https://objects.example/upload", Method: http.MethodPut, Headers: map[string]string{"X-Test": "bound"}},
		info: ObjectInfo{Size: 12, SHA256Base64: base64.StdEncoding.EncodeToString(digestRaw), VersionID: "v1"},
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	service := &Server{Store: store, SigningKey: bytes.Repeat([]byte("k"), 32), PrincipalHeader: "X-Principal", Now: func() time.Time { return now }}
	handler, err := service.Handler()
	if err != nil {
		t.Fatal(err)
	}
	createBody := strings.NewReader(`{"run_id":"run-1","bundle_sha256":"` + digest + `","bundle_bytes":12}`)
	create := httptest.NewRequest(http.MethodPost, "/api/v1/roast/runs", createBody)
	create.Header.Set("X-Principal", "user@example.com")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, create)
	if recorder.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var grant uploadGrant
	if err := json.Unmarshal(recorder.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if grant.AttemptID == "" || grant.ObjectKey == "" || grant.RequiredHeaders["X-Test"] != "bound" {
		t.Fatalf("grant = %#v", grant)
	}
	completeBody := strings.NewReader(`{"attempt_id":"` + grant.AttemptID + `","version_id":"v1"}`)
	complete := httptest.NewRequest(http.MethodPost, "/api/v1/roast/runs/run-1/complete", completeBody)
	complete.Header.Set("X-Principal", "user@example.com")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, complete)
	if recorder.Code != http.StatusOK {
		t.Fatalf("complete status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBrokerRejectsSpoofedPrincipalAndTamperedGrant(t *testing.T) {
	digestRaw := bytes.Repeat([]byte{0x42}, 32)
	digest := hex.EncodeToString(digestRaw)
	store := &fakeStore{put: PresignedPut{URL: "https://objects.example/upload", Method: http.MethodPut}, info: ObjectInfo{Size: 12, SHA256Base64: base64.StdEncoding.EncodeToString(digestRaw)}}
	service := &Server{Store: store, SigningKey: bytes.Repeat([]byte("k"), 32), PrincipalHeader: "X-Principal"}
	handler, err := service.Handler()
	if err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRequest(http.MethodPost, "/api/v1/roast/runs", strings.NewReader(`{"run_id":"run-1","bundle_sha256":"`+digest+`","bundle_bytes":12}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, create)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing principal status = %d", recorder.Code)
	}
	create.Header.Set("X-Principal", "alice@example.com")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, create)
	var grant uploadGrant
	_ = json.Unmarshal(recorder.Body.Bytes(), &grant)
	grant.AttemptID = "x" + grant.AttemptID
	complete := httptest.NewRequest(http.MethodPost, "/api/v1/roast/runs/run-1/complete", strings.NewReader(`{"attempt_id":"`+grant.AttemptID+`","version_id":""}`))
	complete.Header.Set("X-Principal", "alice@example.com")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, complete)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("tampered completion status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
