package jig

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The denial log. microsandbox records denied names only at debug level,
// in the sandbox's runtime.log, which at that level grows by about 16 KB/s
// (docs/microsandbox.md). The supervisor follows it, appends each denial to
// <state>/denials.jsonl, one Denial per line, and truncates runtime.log
// once it passes maxRuntimeLog; the runtime appends, so writing simply
// restarts at zero. A line written between our last read and the truncate
// is lost: a window of microseconds every quarter hour or so.

const maxRuntimeLog = 16 << 20

// Denial is one refused connection or DNS query.
type Denial struct {
	Time   time.Time `json:"time"`
	Client string    `json:"client"`
	Method string    `json:"method"`
	Host   string    `json:"host"`
	Port   int       `json:"port"`
	Reason string    `json:"reason"`
}

var denialRE = regexp.MustCompile(`^(\S+)\s+DEBUG microsandbox_network::\S+: ((?:DNS query|TCP egress) denied by [^=]*?)\s+((?:\w+=\S+\s*)+)$`)

// parseDenial reads one runtime.log line, for example
//
//	2026-11-22T23:39:36.768489Z DEBUG microsandbox_network::engine::dns::forwarder: DNS query denied by network policy domain=example.org
//	2026-11-22T23:39:36.765763Z DEBUG microsandbox_network::engine::tcp::proxy: TCP egress denied by strict hostname policy sni="proxy.golang.org" dst=142.251.179.141:443
func parseDenial(line, client string) (Denial, bool) {
	mm := denialRE.FindStringSubmatch(strings.TrimSpace(line))
	if mm == nil {
		return Denial{}, false
	}
	d := Denial{Client: client, Reason: mm[2], Method: "connect"}
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
	var last Denial
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
func drainDenials(src string, off int64, name string, out io.Writer, last *Denial, log *slog.Logger) int64 {
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

// denied prints the sandbox's denied hosts, most recent first, with counts.
func denied(state string) {
	f, err := os.Open(filepath.Join(state, "denials.jsonl"))
	if err != nil {
		fail("no denials recorded (%v)", err)
	}
	defer f.Close()
	type agg struct {
		host  string
		n     int
		ports map[int]bool
		last  time.Time
	}
	byHost := map[string]*agg{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var d Denial
		if json.Unmarshal(sc.Bytes(), &d) != nil || d.Host == "" {
			continue
		}
		a := byHost[d.Host]
		if a == nil {
			a = &agg{host: d.Host, ports: map[int]bool{}}
			byHost[d.Host] = a
		}
		a.n++
		if d.Port != 0 {
			a.ports[d.Port] = true
		}
		if d.Time.After(a.last) {
			a.last = d.Time
		}
	}
	list := make([]*agg, 0, len(byHost))
	for _, a := range byHost {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].last.After(list[j].last) })
	for _, a := range list {
		var ps []string
		for p := range a.ports {
			ps = append(ps, strconv.Itoa(p))
		}
		sort.Strings(ps)
		port := ""
		if len(ps) > 0 {
			port = " port " + strings.Join(ps, ",")
		}
		fmt.Printf("%s  %-40s %4d×%s\n", a.last.Local().Format("2006-01-02 15:04:05"), a.host, a.n, port)
	}
	if len(list) > 0 {
		fmt.Println("\nTo allow a host: add it to a layer file, pass it in JIG_LAYERS, then mono-jig rm and up (rules are fixed at create).")
	}
}
