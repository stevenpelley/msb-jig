package jig

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The broker's mode, chosen when a sandbox is created
// (docs/transparent-broker.md):
//
//   - server (the default): the guest calls the broker by its own names
//     (gitlab.broker.internal, ...) over a vsock route, with git's
//     insteadOf, GH_HOST and the like pointing there;
//   - proxy: the broker is microsandbox's outbound proxy. The guest calls
//     gitlab.com, github.com and api.anthropic.com by their real names;
//     the broker terminates those (its CA is trusted for them alone) and
//     passes every other connection through. AWS too, at the AWS MCP
//     Server's own host, which the broker answers itself. No vsock route.
//
// microsandbox fixes a sandbox's network at create, so the mode is saved
// in <state>/broker-mode, like the mounts, and a sandbox that exists
// refuses another (mono-jig rm first).

const (
	modeServer = "server"
	modeProxy  = "proxy"
	// proxyPasswordEnv carries the proxy's password from the supervisor
	// to microsandbox (SecretSourceEnv): never into the guest.
	proxyPasswordEnv = "JIG_PROXY_PASSWORD"
)

func brokerModeFile(state string) string { return filepath.Join(state, "broker-mode") }

// proxyPortFile holds proxy mode's port, fixed at create: microsandbox
// keeps the proxy's address with the sandbox.
func proxyPortFile(state string) string { return filepath.Join(state, "proxy-port") }

// splitBrokerMode takes --broker <mode> or --broker=<mode> out of args.
func splitBrokerMode(args []string) (rest []string, mode string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--broker" && i+1 < len(args):
			i++
			mode = args[i]
		case strings.HasPrefix(a, "--broker="):
			mode = strings.TrimPrefix(a, "--broker=")
		default:
			rest = append(rest, a)
		}
	}
	return rest, mode
}

// saveBrokerMode records the mode for a sandbox about to be created, or
// checks it against an existing sandbox's.
func saveBrokerMode(name, state, mode string) {
	if mode != modeServer && mode != modeProxy {
		fail("--broker %s: want server or proxy", mode)
	}
	if ok, err := track.Backend.Exists(context.Background(), name); err != nil {
		fail("%v", err)
	} else if ok {
		if have := loadBrokerMode(state); have != mode {
			fail("%s exists with the broker in %s mode, fixed when it was made: mono-jig rm %s, then again with --broker %s", name, have, name, mode)
		}
		return
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		fail("%v", err)
	}
	if err := os.WriteFile(brokerModeFile(state), []byte(mode+"\n"), 0o600); err != nil {
		fail("%v", err)
	}
	_ = os.Remove(proxyPortFile(state))
}

// loadBrokerMode is the sandbox's mode: server unless saved otherwise.
func loadBrokerMode(state string) string {
	b, err := os.ReadFile(brokerModeFile(state))
	if err != nil {
		return modeServer
	}
	if strings.TrimSpace(string(b)) == modeProxy {
		return modeProxy
	}
	return modeServer
}

// listenProxy listens on the sandbox's proxy port on loopback: the one
// saved, or a free one, saved for the next start.
func listenProxy(state string) (net.Listener, error) {
	if b, err := os.ReadFile(proxyPortFile(state)); err == nil {
		port, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			return nil, fmt.Errorf("%s: %v", proxyPortFile(state), err)
		}
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return nil, fmt.Errorf("the sandbox's proxy port, fixed when it was made: %w (free it, or mono-jig rm and create it again)", err)
		}
		return l, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(proxyPortFile(state), []byte(strconv.Itoa(port)+"\n"), 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// newProxyPassword makes this run's password and puts it where
// microsandbox reads it.
func newProxyPassword() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		fail("%v", err)
	}
	pw := hex.EncodeToString(b)
	os.Setenv(proxyPasswordEnv, pw)
	return pw
}
