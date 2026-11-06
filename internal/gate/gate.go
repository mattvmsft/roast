// Package gate installs and manages Roast's native Git pre-commit hook.
package gate

import _ "embed"

// HookScript is the native Git pre-commit hook installed by this package.
//
//go:embed pre-commit.sh
var HookScript string

const (
	HookName    = "pre-commit"
	SidecarName = "roast-pre-commit"
	HookMarker  = "# roast-gate-hook: v1"
	hookHeader  = "#!/bin/sh\n" + HookMarker + "\n"
	appendBegin = "# >>> roast pre-commit gate (managed by `roast gate`) >>>"
	appendEnd   = "# <<< roast pre-commit gate <<<"
)
