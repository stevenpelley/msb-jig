// Package sandbox carries the guest provisioning script for runtimes that
// copy it in at create time (microsandbox); Lima inlines it in its template.
package sandbox

import _ "embed"

// ProvisionScript is provision.sh.
//
//go:embed provision.sh
var ProvisionScript string

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
