// Package guest is what goes into the sandbox VM: provision.sh (run by
// the supervisor at start, and by the image's Dockerfile at build), the
// image's Dockerfile, and mcpcli, the agent's MCP client.
package guest

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

// ProvisionScript is provision.sh.
//
//go:embed provision.sh
var ProvisionScript string

// ImageDockerfile builds the base image for microsandbox sandboxes, with
// provision.sh beside it.
//
//go:embed image.Dockerfile
var ImageDockerfile string

// ProvisionHash identifies this provision.sh, as the base image records it.
func ProvisionHash() string {
	h := sha256.Sum256([]byte(ProvisionScript))
	return hex.EncodeToString(h[:6])
}

// MCPCLI is mcpcli, the agent's MCP client for the broker's AWS MCP
// endpoint, by file name (copied from the sbx-mcp-client skill).
var MCPCLI = map[string]string{"mcpcli": mcpcliWrapper, "mcpcli.py": mcpcliPy, "cwl_guarded_query.py": cwlGuardedQuery}

var (
	//go:embed mcpcli/mcpcli
	mcpcliWrapper string
	//go:embed mcpcli/mcpcli.py
	mcpcliPy string
	//go:embed mcpcli/cwl_guarded_query.py
	cwlGuardedQuery string
)
