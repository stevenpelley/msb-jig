package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mcp-restrict/filter"
)

// The denial log. microsandbox records denied names only at debug level,
// in the sandbox's runtime.log, which at that level grows by about 16 KB/s
// (microsandbox.md). The supervisor follows it, appends each denial to
// <state>/denials.jsonl in the filter's format, and truncates runtime.log
// once it passes maxRuntimeLog; the runtime appends, so writing simply
// restarts at zero. A line written between our last read and the truncate
// is lost: a window of microseconds every quarter hour or so.

const maxRuntimeLog = 16 << 20

var denialRE = regexp.MustCompile(`^(\S+)\s+DEBUG microsandbox_network::\S+: ((?:DNS query|TCP egress) denied by [^=]*?)\s+((?:\w+=\S+\s*)+)$`)

// parseDenial reads one runtime.log line, for example
//
//	2026-11-22T23:39:36.768489Z DEBUG microsandbox_network::engine::dns::forwarder: DNS query denied by network policy domain=example.org
//	2026-11-22T23:39:36.765763Z DEBUG microsandbox_network::engine::tcp::proxy: TCP egress denied by strict hostname policy sni="proxy.golang.org" dst=142.251.179.141:443
func parseDenial(line, client string) (filter.Denial, bool) {
	mm := denialRE.FindStringSubmatch(strings.TrimSpace(line))
	if mm == nil {
		return filter.Denial{}, false
	}
	d := filter.Denial{Client: client, Reason: mm[2], Method: "connect"}
	d.Time, _ = time.Parse(time.RFC3339Nano, mm[1])
	kv := map[string]string{}
	for _, f := range strings.Fields(mm[3]) {
		if k, v, ok := strings.Cut(f, "="); ok {
			kv[k] = strings.Trim(v, `"`)
		}
	}
	if strings.HasPrefix(mm[2], "DNS") {
		d.Method, d.Host = "dns", kv["domain"]
	} else {
		if h, p, err := net.SplitHostPort(kv["dst"]); err == nil {
			d.Host = h
			d.Port, _ = strconv.Atoi(p)
		}
		if kv["sni"] != "" {
			d.Host = kv["sni"]
		}
	}
	return d, d.Host != ""
}

// runtimeLog is the sandbox's runtime log, under microsandbox's home.
func runtimeLog(name string) string {
	home := os.Getenv("MSB_HOME")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".microsandbox")
	}
	return filepath.Join(home, "sandboxes", name, "logs", "runtime.log")
}

// followDenials reads from off (where the log ended before this
// supervisor started the sandbox) until ctx ends or the sandbox stops, then
// reads once more.
func followDenials(ctx context.Context, name, state string, off int64, log *slog.Logger, stopped <-chan struct{}) {
	src := runtimeLog(name)
	out, err := os.OpenFile(filepath.Join(state, "denials.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Error("denial log", "err", err)
		return
	}
	defer out.Close()
	var last filter.Denial
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		off = drainDenials(src, off, name, out, &last, log)
		select {
		case <-ctx.Done():
			<-stopped
		case <-stopped:
		case <-tick.C:
			continue
		}
		drainDenials(src, off, name, out, &last, log)
		return
	}
}

// drainDenials reads src from off to its last full line, records the
// denials, truncates src if it's too big, and returns the new offset.
func drainDenials(src string, off int64, name string, out io.Writer, last *filter.Denial, log *slog.Logger) int64 {
	f, err := os.Open(src)
	if err != nil {
		return 0
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() < off {
		off = 0 // truncated or replaced
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off
	}
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a partial line waits for the next pass
		}
		off += int64(len(line))
		if !bytes.Contains(line, []byte(" denied by ")) {
			continue
		}
		d, ok := parseDenial(string(line), name)
		// The resolver logs each name once per query family: keep one.
		if !ok || (d.Host == last.Host && d.Method == last.Method && d.Time.Sub(last.Time) < time.Second) {
			continue
		}
		*last = d
		b, _ := json.Marshal(d)
		_, _ = out.Write(append(b, '\n'))
		log.Info("denied", "method", d.Method, "host", d.Host, "port", d.Port, "reason", d.Reason)
	}
	if off > maxRuntimeLog {
		if err := os.Truncate(src, 0); err == nil {
			off = 0
		}
	}
	return off
}
