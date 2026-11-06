#!/usr/bin/env bash
# Generate runtime dependency notices for exactly the CGO-disabled release
# targets. The tool is installed for the host, then inspects each target graph.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

targets=("$@")
if [[ ${#targets[@]} -eq 0 ]]; then
  targets=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64)
fi
for target in "${targets[@]}"; do
  case "$target" in
    linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|windows/arm64) ;;
    *) echo "unsupported license target: $target" >&2; exit 1 ;;
  esac
done

# Only replace a bundle this script owns, never a user-provided directory or
# symlink. Stage everything first so failed generation preserves the old one.
marker='roast runtime notices: v1'
if [[ -e THIRD_PARTY_LICENSES || -L THIRD_PARTY_LICENSES ]]; then
  if [[ -L THIRD_PARTY_LICENSES || ! -f THIRD_PARTY_LICENSES/.roast-generated ]]; then
    echo 'refusing to replace an unowned THIRD_PARTY_LICENSES directory' >&2
    exit 1
  fi
  case "$(<THIRD_PARTY_LICENSES/.roast-generated)" in
    "$marker"|'roast runtime notices: go-licenses/v2 v2.0.1') ;;
    *) echo 'refusing to replace an unowned THIRD_PARTY_LICENSES directory' >&2; exit 1 ;;
  esac
fi

license_work=$(mktemp -d "${TMPDIR:-/tmp}/roast-licenses.XXXXXXXX")
trap 'rm -r -- "$license_work"' EXIT
mkdir -p "$license_work/bin" "$license_work/bundle"
env GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" \
  go build -modfile=.github/tools/licenses/go.mod -o "$license_work/bin/go-licenses" \
  github.com/google/go-licenses/v2
license_tool="$license_work/bin/go-licenses"
allowed='Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MIT,MPL-2.0,Unlicense'

for target in "${targets[@]}"; do
  goos=${target%/*}
  goarch=${target#*/}
  printf 'Generating runtime notices for %s\n' "$target"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "$license_tool" check \
    ./cmd/roast ./cmd/roast-broker --ignore github.com/bluesky-social/roast \
    --allowed_licenses "$allowed"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "$license_tool" save \
    ./cmd/roast ./cmd/roast-broker --ignore github.com/bluesky-social/roast \
    --save_path "$license_work/$goos-$goarch"
  # Module-cache licenses are read-only. Normalize only the generated copies
  # so repeated notices from the next target can be merged on macOS too.
  chmod -R u+rwX "$license_work/$goos-$goarch"
  cp -R "$license_work/$goos-$goarch/." "$license_work/bundle/"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "$license_tool" report \
    ./cmd/roast ./cmd/roast-broker --ignore github.com/bluesky-social/roast \
    > "$license_work/bundle/$goos-$goarch.csv"
done

# go-licenses collects md5-simd's module-root Apache license, but its AMD64
# assembly also carries an Igneous Systems MIT grant. Keep the entire grant
# from the selected module source, not a separately maintained copy. Include
# it in every bundle so target subsets and broker images carry the same notice.
md5_module=$(go list -m -f '{{.Dir}}' github.com/minio/md5-simd)
md5_notice="$license_work/bundle/github.com/minio/md5-simd/NOTICE-Igneous-Systems-MIT.txt"
mkdir -p "$(dirname "$md5_notice")"
if ! awk '
  $0 == "// Copyright (c) 2018 Igneous Systems" { copying = 1 }
  copying {
    if ($0 !~ /^\/\//) exit 1
    line = $0
    sub(/^\/\/ ?/, "", line)
    print line
    if ($0 == "//   SOFTWARE.") { complete = 1; exit }
  }
  END { if (!complete) exit 1 }
' "$md5_module/block8_amd64.s" > "$md5_notice"; then
  echo 'cannot collect complete embedded md5-simd MIT notice; inspect the selected dependency source' >&2
  exit 1
fi

# golang.org/x/net/publicsuffix (linked through minio-go's cookie jar) embeds
# data generated from the Public Suffix List, which is MPL-2.0 rather than
# x/net's BSD license. go-licenses collects only the module-root license, so
# record the list revision named by the selected module source and where to
# obtain that revision's source. Fail rather than guess when it is not found.
net_module=$(go list -m -f '{{.Dir}}' golang.org/x/net)
psl_table="$net_module/publicsuffix/table.go"
psl_pattern='^const version = "(publicsuffix\.org.s public_suffix_list\.dat, git revision ([0-9a-f]{40}) \([^)]*\))"$'
psl_version=''
psl_revision=''
if [[ -f "$psl_table" ]]; then
  while IFS= read -r line; do
    if [[ $line =~ $psl_pattern ]]; then
      psl_version=${BASH_REMATCH[1]}
      psl_revision=${BASH_REMATCH[2]}
      break
    fi
  done < "$psl_table"
fi
mpl_text=scripts/notices/MPL-2.0.txt
if [[ -z "$psl_revision" || ! -s "$mpl_text" ]]; then
  echo 'cannot collect the embedded Public Suffix List MPL-2.0 notice; inspect the selected golang.org/x/net source' >&2
  exit 1
fi
psl_notice="$license_work/bundle/golang.org/x/net/NOTICE-publicsuffix-MPL-2.0.txt"
mkdir -p "$(dirname "$psl_notice")"
{
  cat <<EOF
Public Suffix List data in golang.org/x/net/publicsuffix

golang.org/x/net/publicsuffix is linked into this program and embeds data
generated from the Public Suffix List:

  $psl_version

The rest of golang.org/x/net is covered by the LICENSE file in this
directory. The Public Suffix List is subject to the following notice:

  This Source Code Form is subject to the terms of the Mozilla Public
  License, v. 2.0. If a copy of the MPL was not distributed with this
  file, You can obtain one at https://mozilla.org/MPL/2.0/.

The Source Code Form of the list is available from the Public Suffix List
project at https://publicsuffix.org/list/. The revision used here is:

  https://github.com/publicsuffix/list/blob/$psl_revision/public_suffix_list.dat

The complete text of the Mozilla Public License, v. 2.0 follows.

----------------------------------------------------------------------

EOF
  cat "$mpl_text"
} > "$psl_notice"

# The Go runtime and standard library are also part of static binaries.
go_root=$(go env GOROOT)
go_license="$go_root/LICENSE"
if [[ ! -f "$go_license" && -f "$go_root/../LICENSE" ]]; then
  # Homebrew keeps Go's LICENSE above its libexec GOROOT.
  go_license="$go_root/../LICENSE"
fi
if [[ ! -f "$go_license" ]]; then
  echo "cannot find the installed Go toolchain's LICENSE" >&2
  exit 1
fi
cp "$go_license" "$license_work/bundle/Go-LICENSE"
if [[ -f "$go_root/PATENTS" ]]; then
  cp "$go_root/PATENTS" "$license_work/bundle/Go-PATENTS"
fi
chmod -R u+rwX "$license_work/bundle"
printf '%s\n' "$marker" > "$license_work/bundle/.roast-generated"
if [[ -d THIRD_PARTY_LICENSES ]]; then
  rm -r -- THIRD_PARTY_LICENSES
fi
mv "$license_work/bundle" THIRD_PARTY_LICENSES
