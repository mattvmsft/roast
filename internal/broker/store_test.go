package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestPresignPutBindsLengthAndChecksumAsHeaders(t *testing.T) {
	store := testS3Store(t)
	grant, err := store.PresignPut(context.Background(), "runs/principal/run/attempt/bundle.tar.zst", 12345, emptySHA256, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(grant.URL)
	if err != nil {
		t.Fatal(err)
	}
	signed := strings.Split(u.Query().Get("X-Amz-SignedHeaders"), ";")
	for _, want := range []string{"content-length", "x-amz-checksum-sha256"} {
		if !contains(signed, want) {
			t.Fatalf("signed headers %q do not bind %s", signed, want)
		}
	}
	if grant.Headers["Content-Length"] != "12345" {
		t.Fatalf("Content-Length = %q", grant.Headers["Content-Length"])
	}
	if grant.Headers["X-Amz-Checksum-Sha256"] != "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=" {
		t.Fatalf("checksum header = %q", grant.Headers["X-Amz-Checksum-Sha256"])
	}
	for _, name := range signed {
		if name == "host" {
			continue
		}
		found := false
		for required := range grant.Headers {
			if strings.EqualFold(required, name) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("signed header %q was not returned to the client", name)
		}
	}
}

func TestPresignPutSignatureChangesWithContent(t *testing.T) {
	store := testS3Store(t)
	base, err := store.PresignPut(context.Background(), "key", 12345, emptySHA256, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	otherSize, err := store.PresignPut(context.Background(), "key", 12346, emptySHA256, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	otherDigest, err := store.PresignPut(context.Background(), "key", 12345, strings.Repeat("0", 63)+"1", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if signature(t, base.URL) == signature(t, otherSize.URL) {
		t.Fatal("changing content length did not change the signature")
	}
	if signature(t, base.URL) == signature(t, otherDigest.URL) {
		t.Fatal("changing checksum did not change the signature")
	}
}

func TestS3StoreIntegration(t *testing.T) {
	endpoint := os.Getenv("ROAST_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("ROAST_TEST_S3_ENDPOINT is not set")
	}
	accessKey := os.Getenv("ROAST_TEST_S3_ACCESS_KEY")
	secretKey := os.Getenv("ROAST_TEST_S3_SECRET_KEY")
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds: credentials.NewStaticV4(accessKey, secretKey, ""), Secure: u.Scheme == "https",
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	bucket := "roast-broker-test"
	if err := client.MakeBucket(t.Context(), bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.RemoveBucket(context.Background(), bucket) })

	store, err := NewS3Store(S3Config{
		Endpoint: endpoint, Bucket: bucket, Region: "us-east-1",
		AccessKey: accessKey, SecretKey: secretKey, PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("roast broker integration test")
	digest := sha256.Sum256(body)
	digestHex := hex.EncodeToString(digest[:])
	key := "runs/integration/bundle.tar.zst"
	t.Cleanup(func() { _ = client.RemoveObject(context.Background(), bucket, key, minio.RemoveObjectOptions{}) })
	grant, err := store.PresignPut(t.Context(), key, int64(len(body)), digestHex, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), grant.Method, grant.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	for name, value := range grant.Headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("presigned PUT returned %s: %s", resp.Status, raw)
	}
	info, err := store.Stat(t.Context(), key, resp.Header.Get("x-amz-version-id"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(body)) || info.SHA256Base64 != base64.StdEncoding.EncodeToString(digest[:]) {
		t.Fatalf("stored object = %#v", info)
	}
}

func testS3Store(t *testing.T) ObjectStore {
	t.Helper()
	store, err := NewS3Store(S3Config{
		Endpoint: "https://objects.example.com", Bucket: "roast-runs", Region: "us-east-1",
		AccessKey: "example-access", SecretKey: "example-secret", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func signature(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	signature := u.Query().Get("X-Amz-Signature")
	if signature == "" {
		t.Fatal("presigned URL has no signature")
	}
	return signature
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
