#!/usr/bin/env bash
set -e

VERSION=${VERSION:-2.5.0}

echo "Installing golangci-lint (version: $VERSION)..."

# Detect architecture
ARCH=$(uname -m)
case $ARCH in
  x86_64)
    ARCH="amd64"
    ;;
  aarch64|arm64)
    ARCH="arm64"
    ;;
  *)
    echo "Unsupported architecture: $ARCH"
    exit 1
    ;;
esac

# Detect OS
OS=$(uname -s | tr '[:upper:]' '[:lower:]')

ASSET="golangci-lint-${VERSION}-${OS}-${ARCH}"
DOWNLOAD_URL="https://github.com/golangci/golangci-lint/releases/download/v${VERSION}/${ASSET}.tar.gz"

echo "Downloading from: $DOWNLOAD_URL"

TEMP_DIR=$(mktemp -d)
cd "$TEMP_DIR"

curl -sL "$DOWNLOAD_URL" -o golangci-lint.tar.gz
tar -xzf golangci-lint.tar.gz

# The archive extracts into a directory named after the asset.
cp "${ASSET}/golangci-lint" /usr/local/bin/golangci-lint
chmod +x /usr/local/bin/golangci-lint

# Cleanup
cd /
rm -rf "$TEMP_DIR"

# Verify installation
golangci-lint --version

echo "golangci-lint feature installed successfully!"
