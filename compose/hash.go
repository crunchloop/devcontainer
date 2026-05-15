package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

// ConfigHash returns a stable hash of an image identifier plus a
// compose service config. The orchestrator stamps this on every
// container it creates as the dev.containers.config-hash label;
// subsequent Up calls compare the stored hash against a freshly
// computed one to decide whether the service must be recreated.
//
// Inputs that DO affect the hash (semantic differences):
//   - imageID
//   - service.Command / Entrypoint
//   - service.Environment / Labels / Volumes / Ports order
//   - any other field of types.ServiceConfig
//
// Inputs that DO NOT affect the hash (incidental differences):
//   - map iteration order of Environment / Labels / Networks /
//     Mappings (encoding/json sorts map keys by spec)
//   - distinct *string pointers with equal string values
//     (json.Marshal dereferences them before serializing)
//
// Slice order IS semantic — Volumes order, Ports order, Command
// order all affect the hash. Compose treats these as ordered.
//
// Implementation is intentionally minimal: encoding/json does the
// heavy lifting. Probe 1 in design/compose-native.md §11.1 verified
// determinism across 1000 iterations + 500 shuffle trials.
func ConfigHash(imageID string, svc composetypes.ServiceConfig) string {
	type hashInput struct {
		ImageID string                      `json:"image_id"`
		Svc     composetypes.ServiceConfig  `json:"svc"`
	}
	b, err := json.Marshal(hashInput{ImageID: imageID, Svc: svc})
	if err != nil {
		// json.Marshal on this input type cannot fail under any
		// Go version that ships compose-go's types — every field
		// is encoding-friendly. Panic is appropriate: this would
		// indicate a compose-go type incompatibility we'd want to
		// notice loudly in tests, not silently produce a degenerate
		// hash.
		panic("compose.ConfigHash: json.Marshal failed: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
