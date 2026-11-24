// Package sandbox is the VM runtime behind mono-jig: Backend creates,
// starts and removes sandboxes, and Guest is a running one, as the
// supervisor and provisioning use it. Microsandbox is the one built; a
// track that can't use it supplies another (docs/library-split.md).
package sandbox

import (
	"context"
	"time"

	"msb-jig/allowlist"
)

// Backend runs sandboxes.
type Backend interface {
	// Open starts the named sandbox, creating it from spec the first time
	// (created). An existing sandbox keeps what was fixed at create
	// (image, sizes, egress, mounts, disks, the vsock route, the proxy,
	// the patched files); only Env and EnvRemove apply to it.
	Open(ctx context.Context, spec Spec) (g Guest, created bool, err error)
	// Exists reports whether the named sandbox exists.
	Exists(ctx context.Context, name string) (bool, error)
	// Remove deletes the named sandbox and its disks; a missing one is no
	// error.
	Remove(ctx context.Context, name string) error
}

// Spec is a sandbox to create (or start).
type Spec struct {
	Name, Image string
	MemoryMiB   uint32
	CPUs        uint8
	DiskGiB     uint32 // the root disk
	// Env is every session's environment; EnvRemove are names an existing
	// sandbox drops.
	Env       map[string]string
	EnvRemove []string
	// Egress is all the sandbox may reach (TCP host:port; "*." a suffix);
	// everything else, DNS for other names included, is denied.
	Egress []allowlist.Entry
	Mounts []Mount
	// Disks are the sandbox's own disks, by guest path, in GiB.
	Disks map[string]uint32
	// Vsock routes guest port VsockPort to the Unix socket VsockSocket on
	// the Mac (the broker's).
	VsockSocket string
	VsockPort   uint32
	// Files are written into the guest at create, mode 0644 (the broker's
	// CA).
	Files map[string]string
	// Proxy, if set, is where every allowed connection goes: an HTTP
	// CONNECT proxy with Basic auth, its password read from this process's
	// environment variable PasswordEnv at every start.
	Proxy *Proxy
}

// Mount is a folder on the Mac in the guest, owned there by UID.
type Mount struct {
	Host, Guest string
	ReadOnly    bool
	UID         uint32
}

// Proxy is an outbound proxy for the sandbox.
type Proxy struct {
	Addr, User, PasswordEnv string
}

// Guest is a running sandbox.
type Guest interface {
	Exists(ctx context.Context, path string) (bool, error)
	ReadFile(ctx context.Context, path string) (string, error)
	// WriteFile writes a file, creating its directory.
	WriteFile(ctx context.Context, path string, data []byte) error
	// RemoveAll removes a path and anything under it; a missing one is no
	// error.
	RemoveAll(ctx context.Context, path string) error
	// Run runs a shell script as root and returns its output (stdout,
	// then stderr) and exit code.
	Run(ctx context.Context, script string) (out string, code int, err error)
	// Job runs a shell script as root until it ends, without its output.
	Job(ctx context.Context, script string) (code int, err error)
	// Stop stops the sandbox, waiting at most timeout for it; Kill ends it.
	Stop(ctx context.Context, timeout time.Duration) error
	Kill(ctx context.Context) error
	// Wait waits until the sandbox stops, and describes how.
	Wait(ctx context.Context) (string, error)
	Close() error
}
