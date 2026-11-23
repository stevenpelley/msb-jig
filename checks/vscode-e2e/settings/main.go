// Command settings prints VS Code user settings for checks/vscode-e2e:
// "ours" (the harness's, from cli.VSCodeSettings) or "default" (only what
// Remote-SSH needs to find the host), plus the host's platform so
// Remote-SSH doesn't ask.
//
//	go run ./checks/vscode-e2e/settings ours|default <ssh_config> <host>
package main

import (
	"encoding/json"
	"os"

	"mcp-restrict/cli"
)

func main() {
	mode, cfg, host := os.Args[1], os.Args[2], os.Args[3]
	s := map[string]any{"remote.SSH.configFile": cfg}
	if mode == "ours" {
		s = cli.VSCodeSettings(cfg)
	}
	s["remote.SSH.remotePlatform"] = map[string]string{host: "linux"}
	s["workbench.startupEditor"] = "none"
	s["security.workspace.trust.enabled"] = false
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s)
}
