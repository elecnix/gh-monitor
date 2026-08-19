#!/bin/bash
set -e

# cli/gh-extension-precompile invokes a build_script_override with the release
# tag as $1. Local runs get no tag, so fall back to git and then to a literal
# "(devel)" — never stamp an empty string, which would print a blank version.
VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || true)"
fi
if [ -z "$VERSION" ]; then
  VERSION="(devel)"
fi
echo "building version ${VERSION}"

# -s -w still strip the symbol table; -X only adds the version stamp, the one
# piece of provenance the Go toolchain does not embed on its own.
LDFLAGS="-s -w -X github.com/elecnix/gh-monitor/cmd.version=${VERSION}"

mkdir -p dist

# Build for Windows
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$LDFLAGS" -o dist/windows-amd64.exe .

# Build for macOS
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="$LDFLAGS" -o dist/darwin-amd64 .
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="$LDFLAGS" -o dist/darwin-arm64 .

# Build for Linux
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$LDFLAGS" -o dist/linux-amd64 .
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$LDFLAGS" -o dist/linux-arm64 .
