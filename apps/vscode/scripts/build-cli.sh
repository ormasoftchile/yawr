#!/usr/bin/env bash
set -euo pipefail

fail() {
  echo "Yawr CLI debug build failed: $1" >&2
  exit 1
}

find_go() {
  if command -v go >/dev/null 2>&1; then
    command -v go
    return 0
  fi
  for candidate in /opt/homebrew/bin/go /usr/local/go/bin/go /usr/local/bin/go /usr/bin/go "$HOME/go/bin/go"; do
    if [ -x "$candidate" ]; then
      echo "$candidate"
      return 0
    fi
  done
  return 1
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXTENSION_ROOT="${1:-"$(cd "$SCRIPT_DIR/.." && pwd)"}"
EXTENSION_ROOT="$(cd "$EXTENSION_ROOT" 2>/dev/null && pwd || echo "$EXTENSION_ROOT")"
YAWR_REPO="$(cd "$EXTENSION_ROOT/../../runtime" 2>/dev/null && pwd || echo "$EXTENSION_ROOT/../../runtime")"

if [ ! -d "$YAWR_REPO" ]; then
  fail "expected the monorepo runtime at $YAWR_REPO, but it does not exist."
fi

GO_MOD="$YAWR_REPO/go.mod"
CMD_YAWR="$YAWR_REPO/cmd/yawr"
if [ ! -f "$GO_MOD" ] || [ ! -d "$CMD_YAWR" ]; then
  fail "$YAWR_REPO does not look like the Yawr runtime. Expected go.mod and cmd/yawr."
fi

GO_BIN="$(find_go || true)"
if [ -z "$GO_BIN" ]; then
  fail "Go was not found on PATH or in common install locations. Install Go 1.25+ from https://go.dev/dl/ (or brew install go), then reopen VS Code."
fi

echo "Building Yawr CLI using $GO_BIN in $YAWR_REPO."
(
  cd "$YAWR_REPO"
  "$GO_BIN" build -o yawr ./cmd/yawr
) || fail "go build exited with an error. Resolve the Go build error above, then press F5 again."

OUTPUT="$YAWR_REPO/yawr"
if [ ! -f "$OUTPUT" ]; then
  fail "go build reported success but $OUTPUT was not created."
fi

echo "Yawr CLI debug build complete: $OUTPUT"
