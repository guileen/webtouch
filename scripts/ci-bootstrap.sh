#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) asset=linux-x86_64; digest=6930ebf62bd4ea607b98fff052596c6ee564b9835b4ce172c75a3f53ae9d91b7 ;;
  Darwin-x86_64) asset=osx-x86_64; digest=537d73604a344ded6fc94e98e07e529d4fe3e4a0b09e59905353950fafc2a1f7 ;;
  Darwin-arm64) asset=osx-aarch_64; digest=193289af0470c6a1aada357d4fba0bbf8d78bfaac8b5e42ca30af2ef75583de2 ;;
  *) echo 'Unsupported platform for pinned protoc' >&2; exit 1 ;;
esac
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "https://github.com/protocolbuffers/protobuf/releases/download/v35.1/protoc-35.1-${asset}.zip" -o "$tmp/protoc.zip"
actual="$(shasum -a 256 "$tmp/protoc.zip" | awk '{print $1}')"
[ "$actual" = "$digest" ] || { echo 'protoc checksum mismatch' >&2; exit 1; }
mkdir -p "$root/.tools"
unzip -oq "$tmp/protoc.zip" -d "$root/.tools"
GOBIN="$root/.tools/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
GOBIN="$root/.tools/bin" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
"$root/.tools/bin/protoc" --version
"$root/.tools/bin/protoc-gen-go" --version
"$root/.tools/bin/protoc-gen-go-grpc" --version
