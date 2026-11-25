package jig

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/stevenpelley/msb-jig/broker"
	"github.com/stevenpelley/msb-jig/broker/providers"
)

// startBroker serves the broker for c: in server mode on
// <state>/broker.sock, which the guest reaches through the vsock route;
// in proxy mode as the sandbox's outbound proxy on a loopback port
// (c.proxyAddr), with no socket. It returns what it listens on, and a
// function that closes everything.
func startBroker(ctx context.Context, c *config, log *slog.Logger) (string, func(), error) {
	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	ps, err := providers.Build(ctx, c.broker, c.state, log)
	if err != nil {
		return "", nil, err
	}
	if len(ps) == 0 {
		log.Warn("no broker provider configured (mono-jig login): the broker answers 404")
	}
	if c.proxy {
		// The CA constrained to the real hosts too.
		ca, err := broker.LoadOrCreateCA(filepath.Join(c.state, "broker-ca"), c.name, c.broker.Hosts()...)
		if err != nil {
			return "", nil, err
		}
		pl, err := listenProxy(c.state)
		if err != nil {
			return "", nil, err
		}
		px := broker.NewProxy(&broker.Server{Providers: ps}, ca, c.broker.Hosts(), newProxyPassword(), log)
		go func() { _ = px.Serve(pl) }()
		closers = append(closers, func() { pl.Close() }, func() { px.Close() })
		c.proxyAddr = pl.Addr().String()
		log.Info("broker proxy ready", "addr", c.proxyAddr, "hosts", c.broker.Hosts())
		return c.proxyAddr, closeAll, nil
	}
	sock := filepath.Join(c.state, "broker.sock")
	l, err := broker.ListenUnix(sock)
	if err != nil {
		return "", nil, err
	}
	closers = append(closers, func() { os.Remove(sock) })
	srv, err := broker.ServeTLS(l, ps, c.state, c.name, log)
	if err != nil {
		closeAll()
		return "", nil, err
	}
	closers = append(closers, func() { srv.Close() })
	return sock, closeAll, nil
}
