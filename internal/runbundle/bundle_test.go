package runbundle

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestBuildIsDeterministicAcrossDirectoriesAndTimestamps(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, dir := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("metadata\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "result.gz"), []byte("result\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Unix(1, 0)
	newer := time.Unix(2_000_000_000, 0)
	if err := os.Chtimes(filepath.Join(first, "meta.json"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(second, "meta.json"), newer, newer); err != nil {
		t.Fatal(err)
	}
	_, firstDigest, firstSize, err := Build(first)
	if err != nil {
		t.Fatal(err)
	}
	_, secondDigest, secondSize, err := Build(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest || firstSize != secondSize {
		t.Fatalf("bundle mismatch: %s/%d != %s/%d", firstDigest, firstSize, secondDigest, secondSize)
	}
	manifest, err := Verify(filepath.Join(first, BundleFile))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != Format || manifest.Version != Version {
		t.Fatalf("manifest identity = %q v%d", manifest.Format, manifest.Version)
	}
	if len(manifest.Files) != 2 || manifest.Files[0].Path != "meta.json" || manifest.Files[1].Path != "result.gz" {
		t.Fatalf("manifest files = %#v", manifest.Files)
	}
}

func TestBuildReusesExistingBundle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, digest, size, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	path2, digest2, size2, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if path2 != path || digest2 != digest || size2 != size {
		t.Fatalf("cached bundle changed: %s %s %d", path2, digest2, size2)
	}
	if runtime.GOOS != "windows" {
		assertMode(t, path, 0o600)
	}
}

func TestBuildPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("private metadata"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, _, _, err := Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		assertMode(t, dir, 0o700)
		assertMode(t, path, 0o600)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Mode != 0o600 {
			t.Fatalf("archive entry %s mode = %o, want 600", hdr.Name, hdr.Mode)
		}
	}
}

func TestBuildRejectsSymlinkedBundle(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "meta.json"), []byte("metadata"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, _, _, err := Build(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(dir, BundleFile)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, _, err := Build(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Build() error = %v, want symlink rejection", err)
	}
	if runtime.GOOS != "windows" {
		assertMode(t, path, 0o644)
	}
}

func TestBundleLockPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not describe Windows ACLs")
	}
	path := filepath.Join(t.TempDir(), "bundle.tmp")
	if err := acquireBundleLock(path); err != nil {
		t.Fatal(err)
	}
	assertMode(t, path, 0o600)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func TestBuildRejectsSymlinkedArtifact(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte("must not be bundled"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "result.json.gz")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, _, err := Build(dir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Build() error = %v, want symlink rejection", err)
	}
}

func TestVerifyRejectsManifestDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, BundleFile)
	manifest := Manifest{
		Format: Format, Version: Version,
		Files: []ManifestEntry{{Path: "meta.json", SHA256: strings.Repeat("0", 64), Size: 4}},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeBundle(path, []archiveFile{
		{name: ManifestFile, data: manifestData},
		{name: "meta.json", data: []byte("meta")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path); err == nil || !strings.Contains(err.Error(), "digest does not match") {
		t.Fatalf("Verify() error = %v, want digest mismatch", err)
	}
}

func TestVerifyAcceptsLegacyNULRegularFileType(t *testing.T) {
	path := filepath.Join(t.TempDir(), BundleFile)
	data := []byte("metadata\n")
	sum := sha256.Sum256(data)
	manifestData, err := json.Marshal(Manifest{
		Format: Format, Version: Version,
		Files: []ManifestEntry{{Path: "meta.json", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for _, file := range []archiveFile{{name: ManifestFile, data: manifestData}, {name: "meta.json", data: data}} {
		// A NUL type flag is the historical alternate spelling of a regular file.
		hdr := &tar.Header{Name: file.name, Typeflag: 0, Mode: 0o644, Size: int64(len(file.data))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path); err != nil {
		t.Fatalf("Verify() rejected legacy regular file type: %v", err)
	}
}

func TestBuildRejectsInvalidExistingBundle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, BundleFile), []byte("not a bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Build(dir); err == nil || !strings.Contains(err.Error(), "verify existing bundle") {
		t.Fatalf("Build() error = %v, want existing bundle verification failure", err)
	}
}
