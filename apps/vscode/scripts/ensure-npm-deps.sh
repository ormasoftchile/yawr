#!/usr/bin/env bash
set -euo pipefail

fail() {
  echo "Yawr extension dependency bootstrap failed: $1" >&2
  exit 1
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXTENSION_ROOT="${1:-"$(cd "$SCRIPT_DIR/.." && pwd)"}"
EXTENSION_ROOT="$(cd "$EXTENSION_ROOT" 2>/dev/null && pwd || echo "$EXTENSION_ROOT")"

PACKAGE_JSON="$EXTENSION_ROOT/package.json"
PACKAGE_LOCK="$EXTENSION_ROOT/package-lock.json"
NODE_MODULES="$EXTENSION_ROOT/node_modules"
INSTALLED_LOCK="$NODE_MODULES/.package-lock.json"

if [ ! -f "$PACKAGE_JSON" ]; then
  fail "package.json was not found at $PACKAGE_JSON. Open the Yawr repository root before pressing F5."
fi

if [ ! -f "$PACKAGE_LOCK" ]; then
  fail "package-lock.json was not found at $PACKAGE_LOCK. This repository expects reproducible installs via npm ci."
fi

if ! command -v npm >/dev/null 2>&1; then
  fail "npm was not found on PATH. Install Node.js 20+ from https://nodejs.org/ and reopen VS Code."
fi

reason=""
if [ ! -d "$NODE_MODULES" ]; then
  reason="node_modules is missing"
elif [ ! -f "$INSTALLED_LOCK" ]; then
  reason="node_modules/.package-lock.json is missing"
elif [ "$PACKAGE_JSON" -nt "$INSTALLED_LOCK" ] || [ "$PACKAGE_LOCK" -nt "$INSTALLED_LOCK" ]; then
  reason="package.json or package-lock.json is newer than the installed dependency lock"
fi

if [ -z "$reason" ]; then
  echo "Yawr extension dependencies are already installed; skipping npm ci."
  exit 0
fi

echo "Installing Yawr extension dependencies with npm ci because $reason."
(
  cd "$EXTENSION_ROOT"
  npm ci
) || fail "npm ci exited with an error. Resolve the npm error above, then press F5 again."

echo "Yawr extension dependency bootstrap complete."
