#!/bin/sh
# roast-gate-hook: v1
# Managed by `roast gate` — this file is regenerated on refresh, so local
# edits are lost. To stop the gate in this repo: `roast gate disable`.
#
# Native git pre-commit gate. git runs this AFTER assembling the commit's
# index and points GIT_INDEX_FILE at the temporary index holding exactly that
# commit's content — so for a path-scoped `git commit <path>` in a dirty tree,
# `roast review --staged` sees that path's content and nothing else. Never
# clear or override GIT_INDEX_FILE here; inheriting it IS the mechanism.
#
# Fails the commit ONLY when roast exits 1 (a finding at or above the
# threshold). Every infrastructure failure — no roast binary, no inference
# credentials, endpoint unreachable, timeout, empty stdout (which means the review never
# produced a result, never that the code is clean) — prints one warning line
# and exits 0. A gate that blocks commits when inference is down is worse than
# no gate.
#
# Env:
#   ROAST_SKIP=1            bypass this commit entirely
#   ROAST_BIN=<path>        use this roast binary instead of PATH
#   ROAST_GATE_TIMEOUT=<s>  watchdog, seconds (default 180; 0 disables)
#   ROAST_GATE_SEVERITY=<s> blocking threshold (default HIGH)

set -u

warn() { printf 'roast gate: %s\n' "$*" >&2; }

# Bypass. Any non-empty value works; ROAST_SKIP=1 is the documented spelling.
if [ -n "${ROAST_SKIP:-}" ]; then
  exit 0
fi

# A dispatcher can be shared through core.hooksPath. Its presence is not
# enrollment: only this repository's local decision can authorize a review.
# Match the aliases accepted by gate.readDecision, ignoring inherited config.
decision=$(git config --local --get roast.gate 2>/dev/null |
  tr '[:upper:]' '[:lower:]' |
  sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
case "$decision" in
  enabled | enable | true | yes | on | 1) ;;
  *) exit 0 ;;
esac

ROAST="${ROAST_BIN:-}"
if [ -z "$ROAST" ]; then
  ROAST=$(command -v roast 2>/dev/null) || ROAST=""
fi
if [ -z "$ROAST" ] || [ ! -x "$ROAST" ]; then
  warn "roast binary not found; commit allowed WITHOUT review"
  exit 0
fi

# git hands us GIT_INDEX_FILE absolute in practice, but a relative value would
# be resolved against each git child's own directory rather than ours — roast
# runs its git queries with `-C <repo root>`, which is this directory, so the
# two agree today and would keep agreeing after an absolutize. Do it anyway:
# the cost is nothing and a silent resolution against the wrong directory
# would review the wrong index.
if [ -n "${GIT_INDEX_FILE:-}" ]; then
  case "$GIT_INDEX_FILE" in
    /*) ;;
    *) GIT_INDEX_FILE="$(pwd)/$GIT_INDEX_FILE" ; export GIT_INDEX_FILE ;;
  esac
fi

# Absolute TMPDIR before mktemp: the hook runs inside an arbitrary repo, and a
# relative TMPDIR would drop captured output into the working tree where
# repo-controlled files or symlinks could interfere.
TMPDIR="${TMPDIR:-/tmp}"
case "$TMPDIR" in
  /*) ;;
  *) TMPDIR=/tmp ;;
esac
export TMPDIR

work=$(mktemp -d "${TMPDIR%/}/roast-gate.XXXXXX") || {
  warn "could not create a temp dir; commit allowed WITHOUT review"
  exit 0
}
trap 'rm -rf "$work"' EXIT
out="$work/out.json"
err="$work/err.md"

timeout_s="${ROAST_GATE_TIMEOUT:-180}"
case "$timeout_s" in
  '' | *[!0-9]*) timeout_s=180 ;;
esac

warn "reviewing the staged content of this commit (ROAST_SKIP=1 bypasses)..."

# stdout is roast's JSON result, stderr its markdown — captured, not streamed,
# so a passing commit stays quiet and a failing one prints the findings once.
# ROAST_NO_HOOK keeps the child from trying to bootstrap a hook of its own.
ROAST_NO_HOOK=1 "$ROAST" review --staged --quick --format both \
  --max-severity "${ROAST_GATE_SEVERITY:-HIGH}" \
  --task "pre-commit gate" >"$out" 2>"$err" &
roast_pid=$!

# Portable watchdog: `timeout(1)` is GNU coreutils and absent on stock macOS.
# The poll loop exits as soon as roast does, so no stray long `sleep` outlives
# the hook. SIGTERM is advisory — roast handles it, but a wedged process might
# not — and the `wait` below has no deadline of its own, so escalate to
# SIGKILL after a grace period or the timeout would not actually bound
# anything.
watchdog_pid=''
if [ "$timeout_s" -gt 0 ]; then
  (
    i=0
    while [ "$i" -lt "$timeout_s" ]; do
      kill -0 "$roast_pid" 2>/dev/null || exit 0
      sleep 1
      i=$((i + 1))
    done
    kill -0 "$roast_pid" 2>/dev/null || exit 0
    : >"$work/timed-out"
    kill -TERM "$roast_pid" 2>/dev/null
    j=0
    while [ "$j" -lt 5 ]; do
      kill -0 "$roast_pid" 2>/dev/null || exit 0
      sleep 1
      j=$((j + 1))
    done
    kill -KILL "$roast_pid" 2>/dev/null
  ) >/dev/null 2>&1 </dev/null &
  watchdog_pid=$!
fi

rc=0
wait "$roast_pid" || rc=$?

if [ -n "$watchdog_pid" ]; then
  kill -TERM "$watchdog_pid" 2>/dev/null
  wait "$watchdog_pid" 2>/dev/null
fi

if [ -f "$work/timed-out" ]; then
  warn "review timed out after ${timeout_s}s; commit allowed WITHOUT review (raise ROAST_GATE_TIMEOUT)"
  exit 0
fi

# Empty stdout means roast never emitted a result: it died, could not
# authenticate, or was killed. That is an infrastructure failure, never a
# clean review — say so explicitly rather than letting silence read as a pass.
if [ ! -s "$out" ]; then
  warn "review produced no result (exit $rc); commit allowed WITHOUT review"
  if [ -s "$err" ]; then
    sed -n '1,20p' "$err" >&2
  fi
  exit 0
fi

case "$rc" in
  0)
    exit 0
    ;;
  1)
    # Findings at or above the threshold — handled below.
    ;;
  *)
    warn "review failed (exit $rc); commit allowed WITHOUT review"
    if [ -s "$err" ]; then
      sed -n '1,40p' "$err" >&2
    fi
    exit 0
    ;;
esac

cat "$err" >&2
warn "commit blocked by the findings above. Fix them, or bypass this one commit with: ROAST_SKIP=1 git commit ..."
exit 1
