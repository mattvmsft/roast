// Package buildinfo carries version metadata stamped at release build time
// via ldflags. Builds without ldflags (`go install`, `go build`) fall back to
// the module version and VCS stamp the Go toolchain embeds; when neither is
// available all three vars keep sentinel values so callers can detect an
// unversioned binary.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

// Version is the semver tag (e.g. "v0.4.2"). Set via
//
//	-ldflags "-X github.com/bluesky-social/roast/internal/buildinfo.Version=v0.1.0"
var Version = "dev"

// Commit is the git commit hash (short). Set via ldflags.
var Commit = "unknown"

// Date is the build date (RFC 3339). Set via ldflags.
var Date = "unknown"

const (
	unsetVersion = "dev"
	unsetValue   = "unknown"
	shortCommit  = 7
)

func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	Version, Commit, Date = resolve(Version, Commit, Date, info)
}

// resolve fills only the values ldflags left at their sentinels, so a
// stamped release build always wins over the toolchain's embedded metadata.
func resolve(version, commit, date string, info *debug.BuildInfo) (string, string, string) {
	if info == nil {
		return version, commit, date
	}
	if version == unsetVersion {
		// "(devel)" means the toolchain had no module or VCS version to embed.
		if v := info.Main.Version; v != "" && v != "(devel)" {
			version = v
		}
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if commit == unsetValue && setting.Value != "" {
				commit = setting.Value[:min(len(setting.Value), shortCommit)]
			}
		case "vcs.time":
			if date == unsetValue && setting.Value != "" {
				date = setting.Value
			}
		}
	}
	return version, commit, date
}

// String returns a one-line human-readable version string.
// In development builds: "dev (commit unknown)"
// In `go install` builds: "v0.1.0 (commit unknown)"
// In release builds:      "v0.1.0 (abc1234, 2026-09-19)"
func String() string {
	if Version == unsetVersion || Date == unsetValue {
		return fmt.Sprintf("%s (commit %s)", Version, Commit)
	}
	return fmt.Sprintf("%s (%s, %s)", Version, Commit, Date)
}
