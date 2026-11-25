package editor

// Editor is the editor on the Mac that opens a sandbox over SSH, with a
// profile of its own (settings, extensions) under the harness's state
// directory, apart from the user's own. VSCode is the personal track's; a
// track picks one (docs/library-split.md).
type Editor interface {
	Name() string
	// Find is its command line on PATH, or an error saying how to get it.
	Find() (string, error)
	// Configure writes its profile's settings under root.
	Configure(root string, p Profile) error
	// Install installs what its profile needs (at setup).
	Install(root string) error
	// Open replaces this process with the editor on dir in host.
	Open(root string, p Profile, host, dir string) error
}

// Profile is what an editor's settings need.
type Profile struct {
	SSHConfig  string   // the SSH config with every sandbox's Host block
	Hosts      []string // the sandboxes' SSH host names
	Extensions []string // the agent's, installed on the remote
}

// GuestSSHD is an Editor that needs OpenSSH's sshd in the guest, reached
// through the backend's own SSH with ssh -W (a TCP forward). Kiro's Remote
// SSH reaches its server only through a Unix-socket forward
// (direct-streamlocal), which msb ssh serve refuses (docs/design.md).
type GuestSSHD interface {
	GuestSSHD() bool
}
