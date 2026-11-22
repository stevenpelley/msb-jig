package netstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
)

// Lima's user-v2 sockets, in $LIMA_HOME/_networks/<name>/ (Lima's
// pkg/networks/usernet/config.go): the vz driver passes a datagram socket's
// fd over <name>_fd.sock, QEMU connects to <name>_qemu.sock, and the host
// agent calls an HTTP API on <name>_ep.sock.
func SocketPaths(dir, name string) (fd, qemu, ep string) {
	return filepath.Join(dir, name+"_fd.sock"), filepath.Join(dir, name+"_qemu.sock"), filepath.Join(dir, name+"_ep.sock")
}

// unixPathMax is the smaller of macOS's (104) and Linux's (108) sun_path.
const unixPathMax = 104

// ServeLima serves the three sockets until ctx ends. The directory is made
// (or kept) 0700, and every connection's peer must be this process's user.
func (n *Network) ServeLima(ctx context.Context, dir, name string) error {
	if err := privateDir(dir); err != nil {
		return err
	}
	fdPath, qemuPath, epPath := SocketPaths(dir, name)
	for _, p := range []string{fdPath, qemuPath, epPath} {
		if len(p) >= unixPathMax {
			return fmt.Errorf("socket path %s is %d bytes; the limit is %d (use a shorter LIMA_HOME)", p, len(p), unixPathMax-1)
		}
	}
	var lns []net.Listener
	for _, p := range []string{fdPath, qemuPath, epPath} {
		_ = os.Remove(p)
		ln, err := net.Listen("unix", p)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return err
		}
		lns = append(lns, &peerListener{Listener: ln, log: n.log})
	}
	go func() {
		<-ctx.Done()
		for _, l := range lns {
			l.Close()
		}
		n.closeForwards()
	}()
	go n.acceptLoop(ctx, lns[0], func(c net.Conn) error {
		fc, err := receiveFDConn(c.(*net.UnixConn))
		c.Close()
		if err != nil {
			return err
		}
		defer fc.Close()
		return n.AcceptBess(ctx, &datagramConn{fc})
	})
	go n.acceptLoop(ctx, lns[1], func(c net.Conn) error {
		defer c.Close()
		return n.AcceptQEMU(ctx, c)
	})
	srv := &http.Server{Handler: n.EndpointHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(lns[2]) }()
	go func() { <-ctx.Done(); srv.Close() }()
	return nil
}

func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by uid %d, not us", dir, st.Uid)
	}
	return os.Chmod(dir, 0o700)
}

func (n *Network) acceptLoop(ctx context.Context, ln net.Listener, handle func(net.Conn) error) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			n.log.Warn("accept failed", "addr", ln.Addr().String(), "err", err)
			continue
		}
		go func() {
			if err := handle(c); err != nil && ctx.Err() == nil {
				n.log.Info("vm connection closed", "addr", ln.Addr().String(), "err", err)
			}
		}()
	}
}

// peerListener drops connections from other users.
type peerListener struct {
	net.Listener
	log interface{ Warn(string, ...any) }
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			c.Close()
			continue
		}
		uid, err := peerUID(uc)
		if err != nil || uid != os.Getuid() {
			l.log.Warn("refused a socket connection from another user", "uid", uid, "err", err)
			c.Close()
			continue
		}
		return c, nil
	}
}

// receiveFDConn reads the one fd Lima's vz driver sends (SCM_RIGHTS) and
// returns it as a connection.
func receiveFDConn(c *net.UnixConn) (net.Conn, error) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	oob := make([]byte, syscall.CmsgSpace(4))
	buf := make([]byte, 64)
	_, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, err
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	var fds []int
	for i := range msgs {
		f, err := syscall.ParseUnixRights(&msgs[i])
		if err != nil {
			return nil, err
		}
		fds = append(fds, f...)
	}
	if len(fds) != 1 {
		for _, fd := range fds {
			syscall.Close(fd)
		}
		return nil, fmt.Errorf("want one fd, got %d", len(fds))
	}
	f := os.NewFile(uintptr(fds[0]), "vm")
	defer f.Close()
	return net.FileConn(f)
}

// datagramConn reports a closed socket as an error, as Lima's UDPFileConn does.
type datagramConn struct{ net.Conn }

func (d *datagramConn) Read(b []byte) (int, error) {
	if err := d.Conn.SetReadDeadline(time.Time{}); errors.Is(err, net.ErrClosed) {
		return 0, err
	}
	return d.Conn.Read(b)
}

// --- the endpoint API Lima's host agent calls ---

// EndpointHandler serves what Lima v2.3's host agent calls on the endpoint
// socket: DHCP leases (to find the VM's address), wait_port, DNS hosts (accepted and
// ignored: our DNS answers only our names), and port forwards, limited to
// SSH from the Mac's loopback into the guest (Lima's fallback when SSH over
// vsock isn't available). Nothing here opens a path out of the VM.
func (n *Network) EndpointHandler() http.Handler {
	mux := http.NewServeMux()
	leases := func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, n.Leases()) }
	mux.HandleFunc("GET /services/dhcp/leases", leases)
	mux.HandleFunc("GET /leases", leases)
	mux.HandleFunc("GET /extension/wait_port", n.waitPort)
	mux.HandleFunc("POST /services/dns/add", func(w http.ResponseWriter, r *http.Request) {
		var z types.Zone
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&z)
		n.log.Info("ignored a DNS zone from Lima", "zone", z.Name)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /services/dns/all", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, []types.Zone{}) })
	mux.HandleFunc("POST /services/forwarder/expose", n.expose)
	mux.HandleFunc("POST /services/forwarder/unexpose", n.unexpose)
	mux.HandleFunc("GET /services/forwarder/all", func(w http.ResponseWriter, _ *http.Request) {
		n.fwMu.Lock()
		defer n.fwMu.Unlock()
		out := []types.ExposeRequest{}
		for _, f := range n.forwards {
			out = append(out, f.req)
		}
		writeJSON(w, out)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (n *Network) waitPort(w http.ResponseWriter, r *http.Request) {
	ip, err := netip.ParseAddr(r.URL.Query().Get("ip"))
	if err != nil {
		http.Error(w, "bad ip", http.StatusBadRequest)
		return
	}
	port, err := strconv.ParseUint(r.URL.Query().Get("port"), 10, 16)
	if err != nil {
		http.Error(w, "bad port", http.StatusBadRequest)
		return
	}
	timeout := 10
	if t := r.URL.Query().Get("timeout"); t != "" {
		if timeout, err = strconv.Atoi(t); err != nil || timeout < 0 {
			http.Error(w, "bad timeout", http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeout)*time.Second)
	defer cancel()
	for {
		c, err := n.DialGuestTCP(ctx, netip.AddrPortFrom(ip, uint16(port)))
		if err == nil {
			c.Close()
			w.WriteHeader(http.StatusOK)
			return
		}
		select {
		case <-ctx.Done():
			http.Error(w, "timed out waiting for "+ip.String()+":"+strconv.Itoa(int(port)), http.StatusRequestTimeout)
			return
		case <-time.After(time.Second):
		}
	}
}

type forward struct {
	req types.ExposeRequest
	ln  net.Listener
}

func (n *Network) expose(w http.ResponseWriter, r *http.Request) {
	var req types.ExposeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	local, err1 := netip.ParseAddrPort(req.Local)
	remote, err2 := netip.ParseAddrPort(req.Remote)
	if err1 != nil || err2 != nil || req.Protocol != types.TCP || !local.Addr().IsLoopback() ||
		remote.Port() != 22 || !n.subnet.Contains(remote.Addr()) || remote.Addr() == n.gateway {
		n.log.Warn("refused a port forward", "local", req.Local, "remote", req.Remote, "protocol", req.Protocol)
		http.Error(w, "investigator-net forwards only tcp from the Mac's loopback to a guest's port 22", http.StatusForbidden)
		return
	}
	ln, err := net.Listen("tcp", local.String())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n.fwMu.Lock()
	if n.forwards == nil {
		n.forwards = map[string]*forward{}
	}
	if old := n.forwards[req.Local]; old != nil {
		old.ln.Close()
	}
	n.forwards[req.Local] = &forward{req: req, ln: ln}
	n.fwMu.Unlock()
	n.log.Info("forwarding ssh", "local", req.Local, "remote", req.Remote)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				g, err := n.DialGuestTCP(ctx, remote)
				cancel()
				if err != nil {
					return
				}
				defer g.Close()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(g, c); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, g); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	w.WriteHeader(http.StatusOK)
}

func (n *Network) unexpose(w http.ResponseWriter, r *http.Request) {
	var req types.UnexposeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	n.fwMu.Lock()
	defer n.fwMu.Unlock()
	if f := n.forwards[req.Local]; f != nil {
		f.ln.Close()
		delete(n.forwards, req.Local)
	}
	w.WriteHeader(http.StatusOK)
}

func (n *Network) closeForwards() {
	n.fwMu.Lock()
	defer n.fwMu.Unlock()
	for k, f := range n.forwards {
		f.ln.Close()
		delete(n.forwards, k)
	}
}
