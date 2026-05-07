package config

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// DevcontainerID returns a stable identifier for a workspace, derived
// deterministically from (localWorkspaceFolder, configPath). Same inputs
// always yield the same id, distinct inputs (with overwhelming probability)
// yield distinct ids.
//
// The id is 16 lowercase base32 characters (80 bits) — collision-resistant
// for any practical workspace count, charset-compliant for use in container
// names, network names, and compose project names.
func DevcontainerID(localWorkspaceFolder, configPath string) string {
	sum := sha256.Sum256([]byte(localWorkspaceFolder + "\x00" + configPath))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:10])
	return strings.ToLower(encoded)
}
