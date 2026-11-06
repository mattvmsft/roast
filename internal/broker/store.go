package broker

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var ErrObjectNotFound = errors.New("object not found")

type PresignedPut struct {
	URL     string
	Method  string
	Headers map[string]string
}

type ObjectInfo struct {
	Size         int64
	SHA256Base64 string
	VersionID    string
}

type ObjectStore interface {
	PresignPut(context.Context, string, int64, string, time.Duration) (PresignedPut, error)
	Stat(context.Context, string, string) (ObjectInfo, error)
}

type S3Config struct {
	Endpoint, Bucket, Region           string
	AccessKey, SecretKey, SessionToken string
	PathStyle                          bool
}

type s3Store struct {
	client *minio.Client
	bucket string
}

func NewS3Store(cfg S3Config) (ObjectStore, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid S3-compatible endpoint %q", cfg.Endpoint)
	}
	var creds *credentials.Credentials
	if cfg.AccessKey == "" && cfg.SecretKey == "" {
		creds = credentials.NewIAM("")
	} else {
		if cfg.AccessKey == "" || cfg.SecretKey == "" {
			return nil, errors.New("both S3 access key and secret key are required")
		}
		creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken)
	}
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds: creds, Secure: u.Scheme == "https", Region: cfg.Region, BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize S3-compatible client: %w", err)
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("S3-compatible bucket is required")
	}
	return &s3Store{client: client, bucket: cfg.Bucket}, nil
}

func (s *s3Store) PresignPut(ctx context.Context, key string, size int64, digest string, ttl time.Duration) (PresignedPut, error) {
	checksum, err := digestBase64(digest)
	if err != nil {
		return PresignedPut{}, err
	}
	headers := http.Header{
		"Content-Length":        []string{strconv.FormatInt(size, 10)},
		"Content-Type":          []string{"application/zstd"},
		"X-Amz-Checksum-Sha256": []string{checksum},
	}
	u, err := s.client.PresignHeader(ctx, http.MethodPut, s.bucket, key, ttl, nil, headers)
	if err != nil {
		return PresignedPut{}, fmt.Errorf("presign upload: %w", err)
	}
	required := make(map[string]string, len(headers))
	for name, values := range headers {
		if len(values) > 0 {
			required[name] = values[0]
		}
	}
	return PresignedPut{URL: u.String(), Method: http.MethodPut, Headers: required}, nil
}

func (s *s3Store) Stat(ctx context.Context, key, versionID string) (ObjectInfo, error) {
	opts := minio.StatObjectOptions{VersionID: versionID, Checksum: true}
	info, err := s.client.StatObject(ctx, s.bucket, key, opts)
	if err != nil {
		response := minio.ToErrorResponse(err)
		if response.StatusCode == http.StatusNotFound || response.Code == "NoSuchKey" || response.Code == "NoSuchVersion" {
			return ObjectInfo{}, ErrObjectNotFound
		}
		return ObjectInfo{}, fmt.Errorf("stat object: %w", err)
	}
	return ObjectInfo{Size: info.Size, SHA256Base64: info.ChecksumSHA256, VersionID: info.VersionID}, nil
}

func digestBase64(digest string) (string, error) {
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return "", errors.New("sha256 must be 64 lowercase hexadecimal characters")
	}
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != 32 {
		return "", errors.New("sha256 must be 64 lowercase hexadecimal characters")
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
