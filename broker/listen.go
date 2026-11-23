package broker

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// CAFile is where a sandbox's CA certificate lives (its key is beside it).
func CAFile(state string) string { return filepath.Join(state, "broker-ca", "ca.pem") }

// ServeTLS serves the providers over TLS on l, under the sandbox's CA, in
// the background. Close the returned server to stop.
func ServeTLS(l net.Listener, providers map[string]http.Handler, state, name string, log *slog.Logger) (*http.Server, error) {
	ca, err := LoadOrCreateCA(filepath.Join(state, "broker-ca"), name)
	if err != nil {
		return nil, fmt.Errorf("ca: %w", err)
	}
	hosts := make([]string, 0, len(providers))
	for h := range providers {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	// No WriteTimeout: model responses stream for minutes.
	srv := &http.Server{Handler: &Server{Providers: providers}, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(tls.NewListener(l, ca.TLSConfig(hosts))) }()
	log.Info("broker ready", "hosts", hosts, "ca", CAFile(state))
	return srv, nil
}

// ListenUnix listens on a Unix socket only the user can reach: mode 0600,
// in a 0700 directory. A stale socket from a killed process is replaced.
func ListenUnix(sock string) (net.Listener, error) {
	dir := filepath.Dir(sock)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// TokenSource returns a credential getter: the command's output, cached
// for a minute, or $env when there is no command.
func TokenSource(command, env string) func() (string, error) {
	if command == "" {
		return func() (string, error) {
			if env != "" {
				if t := os.Getenv(env); t != "" {
					return t, nil
				}
			}
			return "", errors.New(env + " is not set")
		}
	}
	var mu sync.Mutex
	var tok string
	var at time.Time
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if tok != "" && time.Since(at) < time.Minute {
			return tok, nil
		}
		out, err := exec.Command("/bin/sh", "-c", command).Output()
		if err != nil {
			return "", fmt.Errorf("token command: %w", err)
		}
		tok, at = strings.TrimSpace(string(out)), time.Now()
		if tok == "" {
			return "", errors.New("token command printed nothing")
		}
		return tok, nil
	}
}
