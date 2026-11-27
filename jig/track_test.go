package jig

import (
	"slices"
	"testing"

	"github.com/stevenpelley/msb-jig/agent"
)

func TestCheckToolCommands(t *testing.T) {
	for _, c := range []struct {
		agent, editor string
		ok            bool
	}{
		{"claude", "vscode", true},
		{"kiro-cli", "kiro", true},
		{"", "vscode", false},
		{"claude", "", false},
		{"kiro", "kiro", false},
		{"shell", "vscode", false},
		{"claude", "open", false},
		{"agent", "vscode", false},
	} {
		if err := checkToolCommands(c.agent, c.editor); (err == nil) != c.ok {
			t.Errorf("checkToolCommands(%q, %q) = %v, want ok %v", c.agent, c.editor, err, c.ok)
		}
	}
}

func TestAgentArgv(t *testing.T) {
	defer func(t0 Track) { track = t0 }(track)
	track = Track{Agent: agent.ClaudeCode{}}
	got := agentArgv("box", []string{"--resume", "a b"})
	want := []string{"msb", "exec", "-t", "-u", "agent", "box", "--", "bash", "-lc", `cd && exec "$0" "$@"`, "claude", "--resume", "a b"}
	if !slices.Equal(got, want) {
		t.Errorf("agentArgv = %q, want %q", got, want)
	}
}
