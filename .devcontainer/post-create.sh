#!/bin/bash
# Post-create script for the devcontainer dev environment.
# Runs once after the container is created.

set -e

cd /workspaces/devcontainer

# Fix ownership of the claude-config volume (Docker named volumes default to root)
sudo chown -R vscode:vscode /home/vscode/.claude

# Persist .claude.json across rebuilds by symlinking into the volume.
# The file lives at ~/.claude.json but the volume only mounts ~/.claude/,
# so without this symlink it's lost on every container rebuild.
if [ ! -L /home/vscode/.claude.json ]; then
  # Migrate existing .claude.json into the volume if present
  if [ -f /home/vscode/.claude.json ]; then
    mv /home/vscode/.claude.json /home/vscode/.claude/.claude.json
  fi

  # Restore from backup if no .claude.json exists in the volume yet
  if [ ! -f /home/vscode/.claude/.claude.json ]; then
    BACKUP=$(ls -t /home/vscode/.claude/backups/.claude.json.backup.* 2>/dev/null | head -1)
    if [ -n "$BACKUP" ]; then
      cp "$BACKUP" /home/vscode/.claude/.claude.json
      echo "Restored .claude.json from backup: $BACKUP"
    fi
  fi

  ln -sf /home/vscode/.claude/.claude.json /home/vscode/.claude.json
  echo "Symlinked ~/.claude.json -> ~/.claude/.claude.json"
fi

# Warm the Go module + build caches so the first `make test` / `make lint`
# is fast. golangci-lint is baked into the image (local feature), so we only
# need to fetch dependencies here.
echo "Downloading Go module dependencies..."
go mod download

echo "Post-create setup complete."
