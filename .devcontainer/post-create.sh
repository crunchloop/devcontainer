#!/bin/bash
# Post-create script for the devcontainer dev environment.
# Runs once after the container is created.

set -e

cd /workspaces/devcontainer

# Warm the Go module cache so the first `make test` / `make lint` is fast.
# golangci-lint is baked into the image (local feature), so we only need to
# fetch dependencies here.
echo "Downloading Go module dependencies..."
go mod download

echo "Post-create setup complete."
