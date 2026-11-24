// Command settings prints VS Code user settings for checks/vscode-e2e:
// "ours" (the harness's, from editor.VSCodeSettings) or "default" (only what
// Remote-SSH needs to find the host), plus the host's platform so
// Remote-SSH doesn't ask.
//
//	go run ./checks/vscode-e2e/settings ours|default <ssh_config> <host> [<json to merge>]
package main

import (
	"encoding/json"
	"os"

	"github.com/stevenpelley/msb-jig/editor"
)

func main() {
	mode, cfg, host := os.Args[1], os.Args[2], os.Args[3]
	s := map[string]any{"remote.SSH.configFile": cfg, "remote.SSH.remotePlatform": map[string]string{host: "linux"}}
	if mode == "ours" {
		s = editor.VSCodeSettings(cfg, []string{host}, []string{"anthropic.claude-code"})
	}
	s["workbench.startupEditor"] = "none"
	s["security.workspace.trust.enabled"] = false
	if len(os.Args) > 4 && os.Args[4] != "" {
		if err := json.Unmarshal([]byte(os.Args[4]), &s); err != nil {
			panic(err)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s)
}
