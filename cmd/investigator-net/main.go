// Command investigator-net is the per-sandbox network process: the VM's
// network stack, the domain filter and the broker (investigator-design.md,
// "Packaging: one process per sandbox"). This version serves the filter on
// a TCP address, for testing tools against it; the netstack and the broker
// come next (prototype-plan.md step 3b).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"mcp-restrict/filter"
)

type layerFlags []string

func (l *layerFlags) String() string     { return strings.Join(*l, ",") }
func (l *layerFlags) Set(v string) error { *l = append(*l, v); return nil }

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "investigator-net: "+f+"\n", a...)
	os.Exit(1)
}

func main() {
	listen := flag.String("filter-listen", "127.0.0.1:3128", "TCP address to serve the filter on")
	profile := flag.String("profile", "investigator", "profile layer: "+strings.Join(filter.Profiles, " or "))
	aclFile := flag.String("acl", "", "an ACL file to use instead of the layers")
	upstream := flag.String("upstream", "", `upstream proxy URL (http://[user:pw@]host:port), or "env" for $HTTPS_PROXY`)
	denials := flag.String("denials", "", "append denials to this file, one JSON object per line")
	var layers layerFlags
	flag.Var(&layers, "layer", "an org or user layer file, in order (repeatable)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	acl, err := loadACL(*aclFile, *profile, layers)
	if err != nil {
		fail("%v", err)
	}
	p := &filter.Proxy{ACL: acl, Log: log}
	if u := *upstream; u != "" {
		if u == "env" {
			if u = os.Getenv("HTTPS_PROXY"); u == "" {
				u = os.Getenv("https_proxy")
			}
		}
		pu, err := url.Parse(u)
		if err != nil || pu.Scheme != "http" || pu.Host == "" {
			fail("upstream %q: want http://host:port", u)
		}
		p.Upstream = pu
	}
	if *denials != "" {
		f, err := os.OpenFile(*denials, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fail("%v", err)
		}
		var mu sync.Mutex
		enc := json.NewEncoder(f)
		p.OnDeny = func(d filter.Denial) { mu.Lock(); _ = enc.Encode(d); mu.Unlock() }
	}
	log.Info("filter listening", "addr", *listen, "entries", len(acl.Entries()), "upstream", p.Upstream.Redacted())
	srv := &http.Server{Addr: *listen, Handler: p, ReadHeaderTimeout: 30 * time.Second}
	fail("%v", srv.ListenAndServe())
}

func loadACL(file, profile string, layers []string) (*filter.ACL, error) {
	var es []filter.Entry
	if file != "" {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if es, err = filter.ParseACL(f); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
	} else {
		ls, err := filter.Layers(profile, layers...)
		if err != nil {
			return nil, err
		}
		if es, err = filter.Compile(ls); err != nil {
			return nil, err
		}
	}
	return filter.NewACL(es)
}
