#!/usr/bin/env bash
set -e

VERSION=${VERSION:-2.1.170}

echo "Installing Claude Code (version: $VERSION)..."
npm install -g "@anthropic-ai/claude-code@$VERSION"

echo "Claude Code installed successfully!"
