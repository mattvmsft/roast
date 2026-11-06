package runbundle

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	// BundleFile is the deterministic local archive built inside a run directory.
	BundleFile = "bundle.tar.zst"
	// ManifestFile is generated inside every bundle. It is not written beside
	// the bundle because its inventory describes the archive, not the mutable
	// run directory.
	ManifestFile = "bundle-manifest.json"
	// Format identifies Roast run bundles independently of their compression or
	// filename.
	Format = "roast-run-bundle"
	// Version is the current bundle manifest schema version.
	Version = 1

	maxManifestBytes = 1 << 20
)

// Manifest is the versioned, deterministic inventory embedded in a bundle.
// Files does not include ManifestFile itself, avoiding a self-hash cycle.
type Manifest struct {
	Format  string          `json:"format"`
	Version int             `json:"version"`
	Files   []ManifestEntry `json:"files"`
}

// ManifestEntry binds one archived path to its exact bytes.
type ManifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type archiveFile struct {
	name string
	data []byte
}

// bundleStaleLockAge is how old a leftover .tmp file must be before
// acquireBundleLock assumes its writer crashed (rather than a concurrent
// process still legitimately writing) and reclaims it. A ~70KB bundle write
// takes well under a second, so this is picked generously above that. The
// goal is to recover from a crashed writer without racing a slow live one.
const bundleStaleLockAge = 10 * time.Minute

// ErrBundleBuildInProgress means another process currently holds the lock
// on this run's bundle build. Callers may retry after that writer finishes.
var ErrBundleBuildInProgress = errors.New("bundle build already in progress by another process")

// Build produces (or reuses) runs/<id>/bundle.tar.zst and returns its
// path, lowercase-hex sha256, and size. It is built ONCE: if the file
// already exists, its bytes are hashed and returned as-is rather than
// rebuilt, so retries reuse identical bytes and the same digest.
//
// The bundle contains every regular file directly in runDir except the bundle
// itself and temporary files. Git snapshot objects live in the repository's
// object store, not this directory.
func Build(runDir string) (path string, sha256hex string, size int64, err error) {
	if err := os.Chmod(runDir, 0o700); err != nil {
		return "", "", 0, fmt.Errorf("protect run directory %s: %w", runDir, err)
	}
	path = filepath.Join(runDir, BundleFile)
	if fi, statErr := os.Lstat(path); statErr == nil && !fi.IsDir() {
		if !fi.Mode().IsRegular() {
			return "", "", 0, fmt.Errorf("existing bundle %s is not a regular file", path)
		}
		// Keep cached bytes/digests intact, but protect older readable bundles.
		if err := os.Chmod(path, 0o600); err != nil {
			return "", "", 0, fmt.Errorf("protect existing bundle %s: %w", path, err)
		}
		if _, verifyErr := Verify(path); verifyErr != nil {
			return "", "", 0, fmt.Errorf("verify existing bundle %s: %w", path, verifyErr)
		}
		sum, hashErr := hashFile(path)
		if hashErr != nil {
			return "", "", 0, fmt.Errorf("hash existing bundle %s: %w", path, hashErr)
		}
		return path, sum, fi.Size(), nil
	}

	tmp := path + ".tmp"
	// Serialize bundle construction across separate Roast processes. Without
	// this, two
	// writers can both os.Create the same tmp inode, interleave writes, and
	// have one process rename out from under the other mid-write -- producing
	// a corrupt archive or a digest that no longer matches the bytes actually
	// on disk, defeating the one-shot content-binding this function exists to
	// provide.
	if err := acquireBundleLock(tmp); err != nil {
		return "", "", 0, err
	}
	files, err := bundleFiles(runDir)
	if err != nil {
		_ = os.Remove(tmp)
		return "", "", 0, err
	}
	if err := writeBundle(tmp, files); err != nil {
		_ = os.Remove(tmp)
		return "", "", 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", "", 0, fmt.Errorf("finalize bundle: %w", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", "", 0, err
	}
	if _, err := Verify(path); err != nil {
		return "", "", 0, fmt.Errorf("verify freshly built bundle %s: %w", path, err)
	}
	sum, err := hashFile(path)
	if err != nil {
		return "", "", 0, fmt.Errorf("hash freshly built bundle %s: %w", path, err)
	}
	return path, sum, fi.Size(), nil
}

// bundleFiles reads each run artifact once, binds those exact bytes into the
// manifest, and returns the complete archive input. Run directories are flat;
// directories and temporary files are excluded, while symlinks and other
// non-regular entries are rejected rather than followed.
func bundleFiles(runDir string) ([]archiveFile, error) {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return nil, fmt.Errorf("read run dir %s: %w", runDir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	files := make([]archiveFile, 0, len(entries)+1)
	manifest := Manifest{Format: Format, Version: Version}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == BundleFile || filepath.Ext(name) == ".tmp" {
			continue
		}
		if name == ManifestFile {
			return nil, fmt.Errorf("run directory contains reserved bundle path %s", ManifestFile)
		}
		data, err := readRegularArtifact(filepath.Join(runDir, name), name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		manifest.Files = append(manifest.Files, ManifestEntry{
			Path: name, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data)),
		})
		files = append(files, archiveFile{name: name, data: data})
	}

	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal bundle manifest: %w", err)
	}
	files = append(files, archiveFile{name: ManifestFile, data: append(manifestData, '\n')})
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

func readRegularArtifact(path, name string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat run artifact %s: %w", name, err)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("run artifact %s is a symlink; only regular files may be bundled", name)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("run artifact %s is not a regular file", name)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open run artifact %s: %w", name, err)
	}
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat open run artifact %s: %w", name, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = f.Close()
		return nil, fmt.Errorf("run artifact %s changed while the bundle was being built", name)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("read run artifact %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close run artifact %s: %w", name, err)
	}
	return data, nil
}

// acquireBundleLock creates tmp exclusively as a cross-process mutex for one
// run's bundle construction: O_EXCL fails if another process's build already
// holds this exact tmp path. A tmp left behind by a crashed writer is
// reclaimed once it is older than bundleStaleLockAge.
func acquireBundleLock(tmp string) error {
	if f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
		return f.Close()
	} else if !os.IsExist(err) {
		return fmt.Errorf("create bundle lock %s: %w", tmp, err)
	}

	fi, err := os.Stat(tmp)
	if err != nil {
		if os.IsNotExist(err) {
			// Raced with the other side finishing (it renamed or removed tmp
			// between our O_EXCL failing and this Stat). Report "in
			// progress" rather than looping: the caller's retry-on-next-sync
			// contract handles this exactly like a live lock holder.
			return ErrBundleBuildInProgress
		}
		return fmt.Errorf("stat bundle lock %s: %w", tmp, err)
	}
	if time.Since(fi.ModTime()) < bundleStaleLockAge {
		return ErrBundleBuildInProgress
	}
	// Stale: reclaim it. Removing then re-creating briefly reopens the same
	// race against any writer that is somehow still alive, but that writer
	// has already exceeded bundleStaleLockAge without finishing a write of a
	// few tens of KB, which in practice means it's dead, not slow.
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reclaim stale bundle lock %s: %w", tmp, err)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ErrBundleBuildInProgress
	}
	return f.Close()
}

// writeBundle writes already-bound archive inputs into a zstd-compressed tar
// at destPath. acquireBundleLock created the file exclusively; opening it for
// truncation here preserves that cross-process lock.
func writeBundle(destPath string, files []archiveFile) error {
	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open bundle: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("protect bundle: %w", err)
	}
	// Closed exactly once, on every path: the happy path falls through to
	// the final f.Close() below; any early return goes through this named
	// cleanup instead so the file descriptor is never leaked or double-closed.
	closeOnErr := true
	defer func() {
		if closeOnErr {
			_ = f.Close()
		}
	}()

	zw, err := zstd.NewWriter(f)
	if err != nil {
		return fmt.Errorf("init zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)

	for _, file := range files {
		hdr := &tar.Header{Name: file.name, Mode: 0o600, Size: int64(len(file.data)), ModTime: time.Unix(0, 0).UTC()}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("tar header %s: %w", file.name, err)
		}
		if _, err := tw.Write(file.data); err != nil {
			return fmt.Errorf("tar write %s: %w", file.name, err)
		}
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("close zstd: %w", err)
	}
	closeOnErr = false
	return f.Close()
}

// Verify validates a bundle's format version, flat regular-file layout, and
// every manifest size and digest. It returns the trusted manifest on success.
func Verify(path string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = f.Close() }()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return Manifest{}, fmt.Errorf("open zstd stream: %w", err)
	}
	defer zr.Close()

	type actualFile struct {
		sha256 string
		size   int64
	}
	actual := make(map[string]actualFile)
	var manifestData []byte
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("read tar: %w", err)
		}
		//lint:ignore SA1019 TypeRegA remains valid when reading legacy tar archives.
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return Manifest{}, fmt.Errorf("bundle entry %q is not a regular file", hdr.Name)
		}
		if hdr.Name == "" || filepath.Base(hdr.Name) != hdr.Name || hdr.Name == "." {
			return Manifest{}, fmt.Errorf("bundle entry %q is not a flat path", hdr.Name)
		}
		if _, duplicate := actual[hdr.Name]; duplicate || (hdr.Name == ManifestFile && manifestData != nil) {
			return Manifest{}, fmt.Errorf("bundle contains duplicate entry %q", hdr.Name)
		}
		if hdr.Name == ManifestFile {
			if hdr.Size > maxManifestBytes {
				return Manifest{}, fmt.Errorf("bundle manifest is larger than %d bytes", maxManifestBytes)
			}
			manifestData, err = io.ReadAll(tr)
			if err != nil {
				return Manifest{}, fmt.Errorf("read bundle manifest: %w", err)
			}
			continue
		}
		h := sha256.New()
		size, err := io.Copy(h, tr)
		if err != nil {
			return Manifest{}, fmt.Errorf("read bundle entry %s: %w", hdr.Name, err)
		}
		actual[hdr.Name] = actualFile{sha256: hex.EncodeToString(h.Sum(nil)), size: size}
	}
	if manifestData == nil {
		return Manifest{}, fmt.Errorf("bundle has no %s", ManifestFile)
	}

	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(manifestData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse bundle manifest: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Manifest{}, errors.New("parse bundle manifest: trailing JSON data")
	}
	if manifest.Format != Format {
		return Manifest{}, fmt.Errorf("unsupported bundle format %q", manifest.Format)
	}
	if manifest.Version != Version {
		return Manifest{}, fmt.Errorf("unsupported bundle version %d", manifest.Version)
	}
	if len(manifest.Files) != len(actual) {
		return Manifest{}, fmt.Errorf("bundle manifest lists %d files but archive contains %d", len(manifest.Files), len(actual))
	}
	previous := ""
	for _, expected := range manifest.Files {
		if expected.Path == "" || filepath.Base(expected.Path) != expected.Path || expected.Path == ManifestFile {
			return Manifest{}, fmt.Errorf("bundle manifest contains invalid path %q", expected.Path)
		}
		if previous != "" && strings.Compare(previous, expected.Path) >= 0 {
			return Manifest{}, errors.New("bundle manifest paths are not strictly sorted")
		}
		previous = expected.Path
		if len(expected.SHA256) != sha256.Size*2 {
			return Manifest{}, fmt.Errorf("bundle manifest has invalid sha256 for %s", expected.Path)
		}
		if _, err := hex.DecodeString(expected.SHA256); err != nil || strings.ToLower(expected.SHA256) != expected.SHA256 {
			return Manifest{}, fmt.Errorf("bundle manifest has invalid sha256 for %s", expected.Path)
		}
		got, ok := actual[expected.Path]
		if !ok {
			return Manifest{}, fmt.Errorf("bundle manifest entry %s is missing from archive", expected.Path)
		}
		if got.size != expected.Size {
			return Manifest{}, fmt.Errorf("bundle entry %s size is %d, manifest says %d", expected.Path, got.size, expected.Size)
		}
		if got.sha256 != expected.SHA256 {
			return Manifest{}, fmt.Errorf("bundle entry %s digest does not match manifest", expected.Path)
		}
	}
	return manifest, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
