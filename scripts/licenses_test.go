package scripts_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLicenseGenerationGuards(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	script, err := os.ReadFile("licenses.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"invalid target", "unowned directory", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			scriptDir := filepath.Join(root, "scripts")
			if err := os.Mkdir(scriptDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(scriptDir, "licenses.sh"), script, 0o600); err != nil {
				t.Fatal(err)
			}
			protected := filepath.Join(root, "THIRD_PARTY_LICENSES")
			target := "linux/amd64"
			if scenario == "invalid target" {
				target = "../invalid"
			} else if scenario == "symlink" {
				protected = t.TempDir()
				if err := os.Symlink(protected, filepath.Join(root, "THIRD_PARTY_LICENSES")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else if err := os.Mkdir(protected, 0o700); err != nil {
				t.Fatal(err)
			}
			if scenario != "invalid target" {
				if err := os.WriteFile(filepath.Join(protected, "keep.txt"), []byte("user data"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(scriptDir, "licenses.sh"), target)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatal("unsafe license generation was accepted")
			}
			want := "refusing to replace"
			if scenario == "invalid target" {
				want = "unsupported license target"
			}
			if !strings.Contains(string(output), want) {
				t.Fatalf("wrong rejection: %s", output)
			}
			if scenario != "invalid target" {
				body, err := os.ReadFile(filepath.Join(protected, "keep.txt"))
				if err != nil || string(body) != "user data" {
					t.Fatal("license generation modified an unowned directory")
				}
			}
		})
	}
}

func TestLicenseBundleOwnershipSurvivesToolUpdates(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	script, err := os.ReadFile("licenses.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"roast runtime notices: v1",
		"roast runtime notices: go-licenses/v2 v2.0.1",
		"roast runtime notices: unknown",
	} {
		t.Run(marker, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"scripts", "bin", "THIRD_PARTY_LICENSES"} {
				if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for path, body := range map[string]string{
				"scripts/licenses.sh":                   string(script),
				"THIRD_PARTY_LICENSES/.roast-generated": marker + "\n",
				"THIRD_PARTY_LICENSES/keep.txt":         "previous bundle",
				// Stop before real tool installation. A failed generation must
				// leave the owned bundle intact, just like an unowned bundle.
				"bin/go": "#!/usr/bin/env bash\necho tool-build-reached >&2\nexit 97\n",
			} {
				if err := os.WriteFile(filepath.Join(root, path), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(root, "scripts/licenses.sh"), "linux/amd64")
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatal("expected simulated tool build failure")
			}
			want := "tool-build-reached"
			if marker == "roast runtime notices: unknown" {
				want = "refusing to replace"
			}
			if !strings.Contains(string(output), want) {
				t.Fatalf("wrong ownership decision: %s", output)
			}
			body, err := os.ReadFile(filepath.Join(root, "THIRD_PARTY_LICENSES/keep.txt"))
			if err != nil || string(body) != "previous bundle" {
				t.Fatal("failed generation modified the previous bundle")
			}
		})
	}
}

// licenseFixture runs licenses.sh for linux/amd64 in a temporary tree whose go
// and go-licenses commands are fakes, so the supplemental-notice extraction is
// exercised without the module cache. edit may change or delete fixture files
// before they are written. It returns the bundle directory and script output.
func licenseFixture(t *testing.T, edit func(files map[string]string)) (string, string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	script, err := os.ReadFile("licenses.sh")
	if err != nil {
		t.Fatal(err)
	}
	mpl, err := os.ReadFile("notices/MPL-2.0.txt")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	files := map[string]string{
		"scripts/licenses.sh":                   string(script),
		"scripts/notices/MPL-2.0.txt":           string(mpl),
		"THIRD_PARTY_LICENSES/.roast-generated": "roast runtime notices: v1\n",
		"THIRD_PARTY_LICENSES/keep.txt":         "previous bundle",
		"goroot/LICENSE":                        "Go license fixture\n",
		"md5-simd/block8_amd64.s":               md5SimdSource,
		"x-net/publicsuffix/table.go":           publicSuffixTable,
		"bin/go": `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  env)
    case "$2" in
      GOHOSTOS) echo linux ;;
      GOHOSTARCH) echo amd64 ;;
      GOROOT) echo "$LICENSE_FIXTURE_ROOT/goroot" ;;
      *) exit 98 ;;
    esac ;;
  build)
    while [[ $# -gt 0 ]]; do
      if [[ "$1" == -o ]]; then
        cp "$LICENSE_FIXTURE_ROOT/license-tool" "$2"
        exit 0
      fi
      shift
    done
    exit 98 ;;
  list)
    for module; do :; done
    case "$module" in
      github.com/minio/md5-simd) echo "$LICENSE_FIXTURE_ROOT/md5-simd" ;;
      golang.org/x/net) echo "$LICENSE_FIXTURE_ROOT/x-net" ;;
      *) exit 98 ;;
    esac ;;
  *) exit 98 ;;
esac
`,
		"license-tool": `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  save)
    while [[ $# -gt 0 ]]; do
      if [[ "$1" == --save_path ]]; then
        mkdir -p "$2/github.com/minio/md5-simd" "$2/golang.org/x/net"
        cp "$LICENSE_FIXTURE_ROOT/goroot/LICENSE" "$2/github.com/minio/md5-simd/LICENSE"
        cp "$LICENSE_FIXTURE_ROOT/goroot/LICENSE" "$2/github.com/minio/md5-simd/NOTICE"
        cp "$LICENSE_FIXTURE_ROOT/goroot/LICENSE" "$2/golang.org/x/net/LICENSE"
        exit 0
      fi
      shift
    done
    exit 98 ;;
  check|report) exit 0 ;;
  *) exit 98 ;;
esac
`,
	}
	if edit != nil {
		edit(files)
	}
	for path, body := range files {
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(root, "scripts/licenses.sh"), "linux/amd64")
	cmd.Env = append(os.Environ(), "LICENSE_FIXTURE_ROOT="+root,
		"PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	return filepath.Join(root, "THIRD_PARTY_LICENSES"), string(output), err
}

// requirePreviousBundle fails unless a failed generation left the previously
// generated bundle in place.
func requirePreviousBundle(t *testing.T, bundle string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(bundle, "keep.txt"))
	if err != nil || string(body) != "previous bundle" {
		t.Fatal("failed generation modified the previous bundle")
	}
}

// The file-level grant in md5-simd's block8_amd64.s is not covered by the
// module-root Apache license collected by go-licenses.
const igneousNotice = `Copyright (c) 2018 Igneous Systems
  MIT License

  Permission is hereby granted, free of charge, to any person obtaining a copy
  of this software and associated documentation files (the "Software"), to deal
  in the Software without restriction, including without limitation the rights
  to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
  copies of the Software, and to permit persons to whom the Software is
  furnished to do so, subject to the following conditions:

  The above copyright notice and this permission notice shall be included in all
  copies or substantial portions of the Software.

  THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
  IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
  FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
  AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
  LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
  OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
  SOFTWARE.
`

var md5SimdSource = "//+build !noasm,!appengine,gc\n\n// " +
	strings.ReplaceAll(strings.TrimSuffix(igneousNotice, "\n"), "\n", "\n// ") +
	"\n\n// Copyright (c) 2020 MinIO Inc. All rights reserved.\n"

const (
	pslRevision = "d6c92f1bbb7433e5db7b8405c25d4035fb8ff376"
	pslVersion  = "publicsuffix.org's public_suffix_list.dat, git revision " + pslRevision + " (2026-02-06T07:36:33Z)"
	// publicSuffixTable mirrors the header x/net's gen.go writes to table.go.
	publicSuffixTable = "// generated by go run gen.go; DO NOT EDIT\n\npackage publicsuffix\n\n" +
		"import _ \"embed\"\n\nconst version = \"" + pslVersion + "\"\n\nconst (\n\tnodesBits = 40\n)\n"
)

func TestLicenseBundleIncludesEmbeddedMITNotice(t *testing.T) {
	for _, scenario := range []string{"complete notice", "missing notice", "truncated notice", "missing source"} {
		t.Run(scenario, func(t *testing.T) {
			bundle, output, err := licenseFixture(t, func(files map[string]string) {
				switch scenario {
				case "missing notice":
					files["md5-simd/block8_amd64.s"] = "// Copyright (c) 2020 MinIO Inc. All rights reserved.\n"
				case "truncated notice":
					files["md5-simd/block8_amd64.s"] = strings.Split(md5SimdSource, "//   SOFTWARE.")[0]
				case "missing source":
					delete(files, "md5-simd/block8_amd64.s")
				}
			})
			if scenario != "complete notice" {
				if err == nil {
					t.Fatal("packaging succeeded without the complete embedded notice")
				}
				if !strings.Contains(output, "embedded md5-simd MIT notice") {
					t.Fatalf("wrong failure: %s", output)
				}
				requirePreviousBundle(t, bundle)
				return
			}
			if err != nil {
				t.Fatalf("generate notices: %v\n%s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(bundle, "github.com/minio/md5-simd/NOTICE-Igneous-Systems-MIT.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != igneousNotice {
				t.Fatalf("embedded notice differs from source:\n%s", body)
			}
			if _, err := os.Stat(filepath.Join(bundle, "github.com/minio/md5-simd/LICENSE")); err != nil {
				t.Fatalf("module-root license lost: %v", err)
			}
			upstreamNotice, err := os.ReadFile(filepath.Join(bundle, "github.com/minio/md5-simd/NOTICE"))
			if err != nil || string(upstreamNotice) != "Go license fixture\n" {
				t.Fatal("module-root notice was replaced by the supplemental grant")
			}
		})
	}
}

func TestLicenseBundleIncludesPublicSuffixListNotice(t *testing.T) {
	mpl, err := os.ReadFile("notices/MPL-2.0.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{
		"complete notice", "missing version", "version without revision", "missing table", "missing MPL text",
	} {
		t.Run(scenario, func(t *testing.T) {
			bundle, output, err := licenseFixture(t, func(files map[string]string) {
				switch scenario {
				case "missing version":
					files["x-net/publicsuffix/table.go"] = strings.Replace(publicSuffixTable, "const version", "const unrelated", 1)
				case "version without revision":
					files["x-net/publicsuffix/table.go"] = strings.Replace(publicSuffixTable, pslVersion, "an explicit version string", 1)
				case "missing table":
					delete(files, "x-net/publicsuffix/table.go")
				case "missing MPL text":
					delete(files, "scripts/notices/MPL-2.0.txt")
				}
			})
			if scenario != "complete notice" {
				if err == nil {
					t.Fatal("packaging succeeded without the Public Suffix List notice")
				}
				if !strings.Contains(output, "embedded Public Suffix List MPL-2.0 notice") {
					t.Fatalf("wrong failure: %s", output)
				}
				requirePreviousBundle(t, bundle)
				return
			}
			if err != nil {
				t.Fatalf("generate notices: %v\n%s", err, output)
			}
			raw, err := os.ReadFile(filepath.Join(bundle, "golang.org/x/net/NOTICE-publicsuffix-MPL-2.0.txt"))
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			for _, want := range []string{
				pslVersion,
				"This Source Code Form is subject to the terms of the Mozilla Public",
				"https://publicsuffix.org/list/",
				"https://github.com/publicsuffix/list/blob/" + pslRevision + "/public_suffix_list.dat",
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("notice lacks %q:\n%s", want, body)
				}
			}
			if !strings.HasSuffix(body, "\n\n"+string(mpl)) {
				t.Fatal("notice does not end with the complete MPL-2.0 text")
			}
			license, err := os.ReadFile(filepath.Join(bundle, "golang.org/x/net/LICENSE"))
			if err != nil || string(license) != "Go license fixture\n" {
				t.Fatal("module-root license was replaced by the supplemental notice")
			}
		})
	}
}

func TestMPLTextIsCanonical(t *testing.T) {
	// SHA-256 of https://www.mozilla.org/media/MPL/2.0/index.txt. The notice
	// must carry the license verbatim, so any edit to this copy is an error.
	// Line endings are normalized for checkouts that convert them.
	const canonical = "3f3d9e0024b1921b067d6f7f88deb4a60cbe7a78e76c64e3f1d7fc3b779b9d04"
	body, err := os.ReadFile("notices/MPL-2.0.txt")
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != canonical {
		t.Fatalf("notices/MPL-2.0.txt SHA-256 = %s, want %s", got, canonical)
	}
}
