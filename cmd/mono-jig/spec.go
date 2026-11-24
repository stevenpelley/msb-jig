package main

import (
	"context"
	"os"
	"path/filepath"

	"mono-jig/broker"
	"mono-jig/sandbox"
)

// openSandbox starts the sandbox through the backend, creating it the
// first time from c. Either way this process owns it. The environment
// applies to an existing sandbox too, so a sandbox from an older
// supervisor, or whose providers changed, follows; the names no longer
// set are removed (staleEnv).
func openSandbox(ctx context.Context, b sandbox.Backend, c config) (sandbox.Guest, bool, error) {
	spec, err := sandboxSpec(c)
	if err != nil {
		return nil, false, err
	}
	g, created, err := b.Open(ctx, spec)
	if err != nil {
		return nil, false, err
	}
	if err := recordEnv(c.state, spec.Env); err != nil {
		g.Close()
		return nil, false, err
	}
	return g, created, nil
}

// sandboxSpec is the sandbox for c.
func sandboxSpec(c config) (sandbox.Spec, error) {
	env := guestConfig(c).Env
	s := sandbox.Spec{
		Name: c.name, Image: c.image, MemoryMiB: c.memory, CPUs: c.cpus, DiskGiB: c.diskGiB,
		Env: env, EnvRemove: staleEnv(c.state, env),
		Egress:      c.egress,
		VsockSocket: filepath.Join(c.state, "broker.sock"), VsockPort: brokerPort,
		Disks: map[string]uint32{},
	}
	for _, mt := range c.mounts {
		s.Mounts = append(s.Mounts, sandbox.Mount{Host: mt.Host, Guest: mt.Guest, ReadOnly: mt.ReadOnly, UID: guestUID})
	}
	if c.dockerGiB > 0 {
		s.Disks[dockerDir] = c.dockerGiB
	}
	// The broker's CA, which provision.sh's broker component trusts.
	if ca, err := os.ReadFile(broker.CAFile(c.state)); err == nil {
		s.Files = map[string]string{guestCA: string(ca)}
	} else if !os.IsNotExist(err) {
		return s, err
	}
	if c.proxy {
		// Every connection the policy allows goes to the broker, with the
		// password, which the backend reads from this process's
		// environment (newProxyPassword) on every start.
		s.Proxy = &sandbox.Proxy{Addr: c.proxyAddr, User: broker.ProxyUser, PasswordEnv: proxyPasswordEnv}
	}
	return s, nil
}
