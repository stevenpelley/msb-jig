// Package sandbox carries the guest provisioning script for runtimes that
// copy it in at create time (microsandbox); Lima inlines it in its template.
package sandbox

import _ "embed"

// ProvisionScript is provision.sh.
//
//go:embed provision.sh
var ProvisionScript string
