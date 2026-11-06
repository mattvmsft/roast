package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	vcs := []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"},
		{Key: "vcs.time", Value: "2026-10-31T12:00:00Z"},
		{Key: "vcs.modified", Value: "false"},
	}
	for _, tc := range []struct {
		name                  string
		version, commit, date string
		info                  *debug.BuildInfo
		want                  [3]string
		wantString            string
	}{
		{
			name:    "go install from the module proxy",
			version: "dev", commit: "unknown", date: "unknown",
			info:       &debug.BuildInfo{Main: debug.Module{Version: "v1.2.0"}},
			want:       [3]string{"v1.2.0", "unknown", "unknown"},
			wantString: "v1.2.0 (commit unknown)",
		},
		{
			name:    "build from a checkout",
			version: "dev", commit: "unknown", date: "unknown",
			info:       &debug.BuildInfo{Main: debug.Module{Version: "v1.2.1-0.20261031120000-0123456789ab"}, Settings: vcs},
			want:       [3]string{"v1.2.1-0.20261031120000-0123456789ab", "0123456", "2026-10-31T12:00:00Z"},
			wantString: "v1.2.1-0.20261031120000-0123456789ab (0123456, 2026-10-31T12:00:00Z)",
		},
		{
			name:    "no embedded module version",
			version: "dev", commit: "unknown", date: "unknown",
			info:       &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: vcs},
			want:       [3]string{"dev", "0123456", "2026-10-31T12:00:00Z"},
			wantString: "dev (commit 0123456)",
		},
		{
			name:    "no build information",
			version: "dev", commit: "unknown", date: "unknown",
			want:       [3]string{"dev", "unknown", "unknown"},
			wantString: "dev (commit unknown)",
		},
		{
			name:    "ldflags-stamped release wins",
			version: "v1.2.0", commit: "abc1234", date: "2026-10-30T00:00:00Z",
			info:       &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}, Settings: vcs},
			want:       [3]string{"v1.2.0", "abc1234", "2026-10-30T00:00:00Z"},
			wantString: "v1.2.0 (abc1234, 2026-10-30T00:00:00Z)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, c, d := resolve(tc.version, tc.commit, tc.date, tc.info)
			if got := [3]string{v, c, d}; got != tc.want {
				t.Fatalf("resolve = %q, want %q", got, tc.want)
			}
			oldVersion, oldCommit, oldDate := Version, Commit, Date
			t.Cleanup(func() { Version, Commit, Date = oldVersion, oldCommit, oldDate })
			Version, Commit, Date = v, c, d
			if got := String(); got != tc.wantString {
				t.Fatalf("String() = %q, want %q", got, tc.wantString)
			}
		})
	}
}
