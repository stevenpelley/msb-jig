package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	m "github.com/superradcompany/microsandbox/sdk/go"

	"mono-jig/allowlist"
)

// Microsandbox runs sandboxes with microsandbox's Go SDK, attached: if
// this process dies, the VM stops with it (docs/microsandbox.md).
type Microsandbox struct{}

func (Microsandbox) Exists(ctx context.Context, name string) (bool, error) {
	_, err := m.GetSandbox(ctx, name)
	switch {
	case err == nil:
		return true, nil
	case m.IsKind(err, m.ErrSandboxNotFound):
		return false, nil
	}
	return false, err
}

func (Microsandbox) Remove(ctx context.Context, name string) error {
	if err := m.RemoveSandbox(ctx, name); err != nil && !m.IsKind(err, m.ErrSandboxNotFound) {
		return err
	}
	return nil
}

func (Microsandbox) Open(ctx context.Context, s Spec) (Guest, bool, error) {
	h, err := m.GetSandbox(ctx, s.Name)
	switch {
	case err == nil:
		if st := h.Status(); st == m.SandboxStatusRunning || st == m.SandboxStatusStarting {
			// Left running by a supervisor that was killed before the parent
			// watch fired, or started by hand: stop it, so we own it.
			if err := exec.Command("msb", "stop", "-t", "30", s.Name).Run(); err != nil {
				return nil, false, fmt.Errorf("%s is %s and couldn't be stopped: %w", s.Name, st, err)
			}
		}
		// The env can change after create (unlike the egress rules), but
		// only while stopped.
		if _, err := h.Modify(ctx, m.ModifyOptions{Env: s.Env, EnvRemove: s.EnvRemove, Policy: m.ModificationPolicyNextStart}); err != nil {
			return nil, false, fmt.Errorf("setting the environment: %w", err)
		}
		sb, err := m.StartSandbox(ctx, s.Name)
		if err != nil {
			return nil, false, err
		}
		return msbGuest{sb}, false, nil
	case m.IsKind(err, m.ErrSandboxNotFound):
	default:
		return nil, false, err
	}
	var opts []m.SandboxOption
	if s.Proxy != nil {
		opts = append(opts, m.WithProxy(m.HTTPConnectProxy(s.Proxy.Addr).Credentials(s.Proxy.User, m.SecretSourceEnv(s.Proxy.PasswordEnv))))
	}
	if s.VsockSocket != "" {
		opts = append(opts, m.WithVsock(m.VsockRoute{HostSocket: s.VsockSocket, Port: s.VsockPort}))
	}
	mode := uint32(0o644)
	for p, body := range s.Files {
		opts = append(opts, m.WithPatches(m.Patch.Text(p, body, m.PatchOptions{Mode: &mode, Replace: true})))
	}
	sb, err := m.CreateSandbox(ctx, s.Name, append(opts,
		m.WithImage(s.Image),
		m.WithMemory(s.MemoryMiB),
		m.WithCPUs(s.CPUs),
		m.WithRootDisk(m.RootDisk.Managed(s.DiskGiB*1024)),
		m.WithNetwork(&m.NetworkConfig{
			DefaultEgress:  m.PolicyActionDeny,
			DefaultIngress: m.PolicyActionDeny,
			Rules:          EgressRules(s.Egress),
			// Strict mode denies HTTPS it can't inspect, i.e. every allowed
			// host without TLS interception. Off, hostname rules match on
			// SNI plus the sandbox's own DNS answers (docs/microsandbox.md).
			DisableStrict: true,
		}),
		m.WithEnv(s.Env),
		// Debug: the only level that records denied names (mono-jig's denials.go).
		m.WithLogLevel(m.LogLevelDebug),
		m.WithMounts(msbMounts(s)),
	)...)
	if err != nil {
		return nil, false, err
	}
	return msbGuest{sb}, true, nil
}

// EgressRules turns the allowlist into microsandbox rules. A "*.suffix"
// entry becomes a domain-suffix rule. No DNS rule: without one,
// microsandbox's resolver answers only allowed names (NXDOMAIN for the
// rest), so DNS can't carry data out; an AllowDNS rule would resolve any
// name.
func EgressRules(entries []allowlist.Entry) []m.PolicyRule {
	var rules []m.PolicyRule
	for _, e := range entries {
		dest := e.Pattern
		if s, ok := strings.CutPrefix(dest, "*."); ok {
			dest = "." + s
		}
		rules = append(rules, m.PolicyRule{Action: m.PolicyActionAllow, Direction: m.PolicyDirectionEgress,
			Destination: dest, Protocol: m.PolicyProtocolTCP, Port: strconv.Itoa(e.Port)})
	}
	return rules
}

// msbMounts are the bind mounts, owned by their UID, and the sandbox's
// own disks.
func msbMounts(s Spec) map[string]m.MountConfig {
	out := map[string]m.MountConfig{}
	for _, mt := range s.Mounts {
		out[mt.Guest] = m.Mount.Bind(mt.Host, m.MountOptions{
			Readonly: mt.ReadOnly,
			Owner:    &m.MountOwner{UID: mt.UID, GID: mt.UID},
		})
	}
	for p, gib := range s.Disks {
		out[p] = m.Mount.Owned(m.OwnedVolumeOptions{Kind: m.VolumeKindDisk, SizeMiB: gib * 1024})
	}
	return out
}

type msbGuest struct{ sb *m.Sandbox }

func (g msbGuest) Exists(ctx context.Context, p string) (bool, error) {
	return g.sb.FS().Exists(ctx, p)
}

func (g msbGuest) ReadFile(ctx context.Context, p string) (string, error) {
	return g.sb.FS().ReadString(ctx, p)
}

func (g msbGuest) WriteFile(ctx context.Context, p string, data []byte) error {
	if err := g.sb.FS().Mkdir(ctx, path.Dir(p)); err != nil {
		return err
	}
	return g.sb.FS().Write(ctx, p, data)
}

func (g msbGuest) RemoveAll(ctx context.Context, p string) error {
	ok, err := g.sb.FS().Exists(ctx, p)
	if err != nil || !ok {
		return err
	}
	if st, err := g.sb.FS().Stat(ctx, p); err == nil && !st.IsDir {
		return g.sb.FS().Remove(ctx, p)
	}
	return g.sb.FS().RemoveDir(ctx, p)
}

func (g msbGuest) Run(ctx context.Context, script string) (string, int, error) {
	h, err := g.sb.ShellStream(ctx, script, m.WithExecUser("root"))
	if err != nil {
		return "", 0, err
	}
	defer h.Close()
	out, err := h.Collect(ctx)
	if err != nil {
		return "", 0, err
	}
	return out.Stdout() + out.Stderr(), out.ExitCode(), nil
}

func (g msbGuest) Job(ctx context.Context, script string) (int, error) {
	h, err := g.sb.ShellStream(ctx, script, m.WithExecUser("root"))
	if err != nil {
		return 0, err
	}
	defer h.Close()
	code, err := h.Wait(ctx)
	return int(code), err
}

func (g msbGuest) Stop(ctx context.Context, timeout time.Duration) error {
	return g.sb.StopWithTimeout(ctx, timeout)
}

func (g msbGuest) Kill(ctx context.Context) error { return g.sb.Kill(ctx) }

func (g msbGuest) Wait(ctx context.Context) (string, error) {
	res, err := g.sb.WaitUntilStopped(ctx)
	return fmt.Sprint(res), err
}

func (g msbGuest) Close() error { return g.sb.Close() }
