package feature

import "fmt"

// runScript is the per-feature wrapper that sources env files and
// invokes the feature's install.sh. Identical structure for every
// feature; only the FEATURE_REF placeholder changes for diagnostic
// output on failure.
const runScriptTemplate = `#!/bin/sh
set -e
trap 'rc=$?; if [ $rc -ne 0 ]; then echo "ERROR: feature %q failed: exit $rc" >&2; fi' EXIT
set -a
. ../builtin.env
. ./feature.env
set +a
chmod +x ./install.sh
./install.sh
`

func generateRunScript(featureRef string) string {
	return fmt.Sprintf(runScriptTemplate, featureRef)
}
