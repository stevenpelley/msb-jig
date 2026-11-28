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
		{"ssh", "vscode", false},
		{"claude", "build", false},
		{"claude", "self", false},
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

func TestSSHArgv(t *testing.T) {
	if got, want := sshArgv("box", nil, false), []string{"msb", "exec", "-t", "-u", "agent", "box", "--", "bash", "-l"}; !slices.Equal(got, want) {
		t.Errorf("shell = %q, want %q", got, want)
	}
	cmd := []string{"ls", "-l"}
	if got, want := sshArgv("box", cmd, false), []string{"msb", "exec", "-u", "agent", "box", "--", "bash", "-lc", `cd && exec "$0" "$@"`, "ls", "-l"}; !slices.Equal(got, want) {
		t.Errorf("no tty = %q, want %q", got, want)
	}
	if got := sshArgv("box", cmd, true); got[2] != "-t" {
		t.Errorf("tty = %q", got)
	}
}
