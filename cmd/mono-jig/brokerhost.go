package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"mono-jig/broker"
	"mono-jig/broker/providers"
)

// startBroker serves the broker for c on <state>/broker.sock (the guest
// reaches it through the vsock route), and in proxy mode also as the
// sandbox's outbound proxy on a loopback port (c.proxyAddr). It returns
// the socket's path, and a function that closes everything.
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
	sock := filepath.Join(c.state, "broker.sock")
	l, err := broker.ListenUnix(sock)
	if err != nil {
		return "", nil, err
	}
	closers = append(closers, func() { os.Remove(sock) })
	if len(ps) == 0 {
		log.Warn("no broker provider configured (mono-jig login): the broker answers 404")
	}
	if c.proxy {
		// The CA first, constrained to the real hosts too: the vsock
		// server below loads the same one.
		ca, err := broker.LoadOrCreateCA(filepath.Join(c.state, "broker-ca"), c.name, c.broker.Hosts()...)
		if err != nil {
			closeAll()
			return "", nil, err
		}
		pl, err := listenProxy(c.state)
		if err != nil {
			closeAll()
			return "", nil, err
		}
		px := broker.NewProxy(&broker.Server{Providers: ps}, ca, c.broker.Hosts(), newProxyPassword(), log)
		go func() { _ = px.Serve(pl) }()
		closers = append(closers, func() { pl.Close() }, func() { px.Close() })
		c.proxyAddr = pl.Addr().String()
		log.Info("broker proxy ready", "addr", c.proxyAddr, "hosts", c.broker.Hosts())
	}
	srv, err := broker.ServeTLS(l, ps, c.state, c.name, log)
	if err != nil {
		closeAll()
		return "", nil, err
	}
	closers = append(closers, func() { srv.Close() })
	return sock, closeAll, nil
}
