#!/usr/bin/env bash
# Scenario 01: compare old vs new zctl output — no imports
# Usage: bash compare.sh
# Requires: go install github.com/zeromicro/go-zero/tools/zctl@latest (auto-installed)
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GOCTL_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"
NEW_GOCTL="$GOCTL_ROOT/bin/zctl"
OLD_GOCTL="$(go env GOPATH)/bin/zctl"
OUT_OLD="$SCRIPT_DIR/output_old"
OUT_NEW="$SCRIPT_DIR/output_new"

verify_build() {
  local dir="$1" label="$2"
  echo "Verifying $label ..."
  cd "$dir"
  go mod tidy
  if go build ./...; then
    echo "  ✅ $label: build passed"
  else
    echo "  ❌ $label: build failed"
    exit 1
  fi
  cd "$SCRIPT_DIR"
}

# Install released zctl and build local zctl
echo ">>> Installing zctl@latest ..."
go install github.com/zeromicro/go-zero/tools/zctl@latest
echo ">>> Building local zctl ..."
go build -o "$NEW_GOCTL" "$GOCTL_ROOT"

# Generate with old zctl
echo ">>> Generating with old zctl ..."
rm -rf "$OUT_OLD" && mkdir -p "$OUT_OLD/pb"
(cd "$OUT_OLD" && go mod init example.com/demo/s01_no_import > /dev/null 2>&1)
cd "$SCRIPT_DIR"
"$OLD_GOCTL" rpc protoc greeter.proto \
  --go_out="$OUT_OLD/pb" \
  --go-grpc_out="$OUT_OLD/pb" \
  --zrpc_out="$OUT_OLD/rpc" \
  --go_opt=paths=source_relative \
  --go-grpc_opt=paths=source_relative \
  --proto_path=.
verify_build "$OUT_OLD" "old"

# Generate with new zctl
echo ">>> Generating with new zctl ..."
rm -rf "$OUT_NEW" && mkdir -p "$OUT_NEW/pb"
(cd "$OUT_NEW" && go mod init example.com/demo/s01_no_import > /dev/null 2>&1)
cd "$SCRIPT_DIR"
"$NEW_GOCTL" rpc protoc greeter.proto \
  --go_out="$OUT_NEW/pb" \
  --go-grpc_out="$OUT_NEW/pb" \
  --zrpc_out="$OUT_NEW/rpc" \
  --go_opt=paths=source_relative \
  --go-grpc_opt=paths=source_relative \
  --proto_path=.
verify_build "$OUT_NEW" "new"

# Diff old vs new (exclude go.mod / go.sum)
echo ""
echo ">>> Diff (old vs new):"
if diff -rq --exclude="go.mod" --exclude="go.sum" "$OUT_OLD" "$OUT_NEW" > /dev/null 2>&1; then
  echo "  [identical] no differences between old and new output"
else
  diff -r --exclude="go.mod" --exclude="go.sum" "$OUT_OLD" "$OUT_NEW" || true
fi
