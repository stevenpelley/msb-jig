package broker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// AWSMCP serves the AWS MCP Server to the sandbox as MCP over plain HTTP,
// for curl and mcpcli; the agent never gets it as a native MCP server.
// Behind it runs AWS's own stdio proxy (mcp-proxy-for-aws, through the
// aws-mcp-proxy wrapper) on the Mac, which holds the credentials, signs
// with SigV4, and switches profiles per call (aws_profile).
//
// One proxy serves every client: the broker initializes it once, answers
// each client's initialize from that, and forwards tools/list and
// tools/call under its own JSON-RPC ids. The proxy keeps no per-client
// state, since the profile is an argument of each call. Sessions are
// optional, so a single curl POST of a tools/call works. Responses are
// plain JSON; GET (the server-to-client stream) is refused, as the spec
// allows.
type AWSMCP struct {
	// Command is the proxy's argv. It speaks MCP over stdin and stdout.
	Command []string
	Env     []string // nil: the broker's environment
	Stderr  io.Writer
	Log     *slog.Logger

	mu      sync.Mutex
	cur     *awsChild     // nil while starting or restarting
	ready   chan struct{} // closed when cur changes
	lastErr error
	nextID  atomic.Int64
}

// AWSMCPPath is the endpoint, on aws.broker.internal.
const AWSMCPPath = "/mcp"

// awsProtocolVersion is what the broker asks the proxy for.
const awsProtocolVersion = "2025-06-18"

type awsChild struct {
	stdin   io.WriteCloser
	wmu     sync.Mutex
	pmu     sync.Mutex
	pending map[int64]chan json.RawMessage
	init    json.RawMessage // the proxy's initialize result
	done    chan struct{}   // closed when the process has exited
}

// Start keeps the proxy running until ctx ends, restarting it if it exits.
func (a *AWSMCP) Start(ctx context.Context) {
	a.mu.Lock()
	if a.ready == nil {
		a.ready = make(chan struct{})
	}
	a.mu.Unlock()
	go func() {
		for {
			err := a.runChild(ctx)
			if ctx.Err() != nil {
				return
			}
			a.log("aws mcp proxy exited; restarting", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
}

func (a *AWSMCP) log(msg string, args ...any) {
	if a.Log != nil {
		a.Log.Info(msg, args...)
	}
}

func (a *AWSMCP) setChild(c *awsChild, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cur, a.lastErr = c, err
	close(a.ready)
	a.ready = make(chan struct{})
}

// child returns the running proxy, waiting up to wait for one.
func (a *AWSMCP) child(ctx context.Context, wait time.Duration) (*awsChild, error) {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		a.mu.Lock()
		c, err, ready := a.cur, a.lastErr, a.ready
		a.mu.Unlock()
		if c != nil {
			return c, nil
		}
		if ready == nil {
			return nil, errors.New("not started")
		}
		select {
		case <-ready:
		case <-t.C:
			if err == nil {
				err = errors.New("still starting")
			}
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// runChild runs one proxy process to its end.
func (a *AWSMCP) runChild(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, a.Command[0], a.Command[1:]...)
	cmd.Env = a.Env
	cmd.Stderr = a.Stderr
	// Its own process group, so stopping it stops what it started (uv runs
	// python as a child). If the broker is killed outright, the proxy's
	// stdin closes and it exits on EOF.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		a.setChild(nil, err)
		return err
	}
	c := &awsChild{stdin: stdin, pending: map[int64]chan json.RawMessage{}, done: make(chan struct{})}
	go func() { c.read(stdout, a.Log); close(c.done) }()

	initErr := a.initialize(ctx, c)
	if initErr != nil {
		a.log("aws mcp proxy initialize failed", "err", initErr)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	} else {
		a.log("aws mcp proxy ready", "pid", cmd.Process.Pid)
		a.setChild(c, nil)
	}
	<-c.done
	err = cmd.Wait()
	if initErr != nil {
		err = initErr
	}
	a.setChild(nil, fmt.Errorf("aws mcp proxy exited: %v", err))
	c.failPending()
	return err
}

func (a *AWSMCP) initialize(ctx context.Context, c *awsChild) error {
	params, _ := json.Marshal(map[string]any{
		"protocolVersion": awsProtocolVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "investigator-broker", "version": "0"},
	})
	ictx, cancel := context.WithTimeout(ctx, 2*time.Minute) // uv may resolve the first time
	defer cancel()
	res, err := c.call(ictx, a.nextID.Add(1), "initialize", params)
	if err != nil {
		return err
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(res, &r) != nil || len(r.Result) == 0 {
		return fmt.Errorf("initialize: %s", res)
	}
	c.init = r.Result
	return c.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
}

func (c *awsChild) write(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

// call sends a request under id and returns the proxy's whole response.
func (c *awsChild) call(ctx context.Context, id int64, method string, params json.RawMessage) (json.RawMessage, error) {
	ch := make(chan json.RawMessage, 1)
	c.pmu.Lock()
	if c.pending == nil {
		c.pmu.Unlock()
		return nil, errors.New("aws mcp proxy exited")
	}
	c.pending[id] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
	}()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if len(params) > 0 {
		msg["params"] = params
	}
	if err := c.write(msg); err != nil {
		return nil, err
	}
	select {
	case res, ok := <-ch:
		if !ok {
			return nil, errors.New("aws mcp proxy exited")
		}
		return res, nil
	case <-c.done:
		return nil, errors.New("aws mcp proxy exited")
	case <-ctx.Done():
		_ = c.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
			"params": map[string]any{"requestId": id, "reason": "client went away"}})
		return nil, ctx.Err()
	}
}

func (c *awsChild) failPending() {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	for _, ch := range c.pending {
		close(ch)
	}
	c.pending = nil
}

// read dispatches the proxy's messages: responses to their callers; pings
// answered; other requests refused; notifications dropped.
func (c *awsChild) read(r io.Reader, log *slog.Logger) {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			c.dispatch(line, log)
		}
		if err != nil {
			return
		}
	}
}

func (c *awsChild) dispatch(line []byte, log *slog.Logger) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		if log != nil {
			log.Warn("aws mcp proxy: not JSON on stdout", "line", string(bytes.TrimSpace(line)))
		}
		return
	}
	switch {
	case m.Method == "ping" && len(m.ID) > 0:
		_ = c.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{}})
	case m.Method != "" && len(m.ID) > 0:
		_ = c.write(map[string]any{"jsonrpc": "2.0", "id": m.ID,
			"error": map[string]any{"code": -32601, "message": "the investigator broker answers no server requests"}})
	case m.Method != "":
		// A notification (progress, logging): nobody to send it to.
	default:
		id, err := strconv.ParseInt(string(m.ID), 10, 64)
		if err != nil {
			return
		}
		c.pmu.Lock()
		ch := c.pending[id]
		c.pmu.Unlock()
		if ch != nil {
			ch <- append(json.RawMessage(nil), line...)
		}
	}
}

func (a *AWSMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != AWSMCPPath {
		httpError(w, http.StatusNotFound, "the AWS MCP endpoint is "+AWSMCPPath)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK) // sessions hold nothing here
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		httpError(w, http.StatusMethodNotAllowed, "POST JSON-RPC messages; there is no server-to-client stream")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBody+1))
	if err != nil || len(body) > maxAPIBody {
		httpError(w, http.StatusRequestEntityTooLarge, "MCP request too large")
		return
	}
	if t := bytes.TrimLeft(body, " \t\r\n"); len(t) > 0 && t[0] == '[' {
		rpcError(w, nil, -32600, "investigator broker: JSON-RPC batches are not accepted")
		return
	}
	var m rpcMessage
	if err := json.Unmarshal(body, &m); err != nil || m.JSONRPC != "2.0" {
		rpcError(w, nil, -32700, "investigator broker: not a JSON-RPC 2.0 message")
		return
	}
	if m.Method == "" {
		rpcError(w, m.ID, -32600, "investigator broker: responses to server requests are not accepted")
		return
	}
	if !mcpMethods[m.Method] {
		a.decision(r, m.Method, "", "", false, "method not allowed")
		rpcError(w, m.ID, -32601, "investigator broker: MCP method "+m.Method+" is not allowed")
		return
	}
	if len(m.ID) == 0 {
		// notifications/initialized and cancelled: the broker initialized
		// the proxy itself, and maps no client ids to cancel.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch m.Method {
	case "ping":
		writeRPCResult(w, m.ID, json.RawMessage("{}"), "")
		return
	case "initialize":
		c, err := a.child(r.Context(), 2*time.Minute)
		if err != nil {
			rpcError(w, m.ID, -32603, "investigator broker: the AWS MCP proxy isn't running: "+err.Error())
			return
		}
		writeRPCResult(w, m.ID, c.init, newSessionID())
		return
	}
	tool, profile := "", ""
	if m.Method == "tools/call" {
		var p struct {
			Name      string `json:"name"`
			Arguments struct {
				Profile string `json:"aws_profile"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil || p.Name == "" {
			rpcError(w, m.ID, -32602, "investigator broker: bad tools/call params")
			return
		}
		tool, profile = p.Name, p.Arguments.Profile
	}
	c, err := a.child(r.Context(), 2*time.Minute)
	if err != nil {
		rpcError(w, m.ID, -32603, "investigator broker: the AWS MCP proxy isn't running: "+err.Error())
		return
	}
	a.decision(r, m.Method, tool, profile, true, "forwarded")
	res, err := c.call(r.Context(), a.nextID.Add(1), m.Method, m.Params)
	if err != nil {
		rpcError(w, m.ID, -32603, "investigator broker: "+err.Error())
		return
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(res, &out); err != nil {
		rpcError(w, m.ID, -32603, "investigator broker: bad response from the AWS MCP proxy")
		return
	}
	out["id"] = m.ID
	w.Header().Set("Content-Type", "application/json")
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
		w.Header().Set("Mcp-Session-Id", sid)
	}
	_ = json.NewEncoder(w).Encode(out)
}

func writeRPCResult(w http.ResponseWriter, id, result json.RawMessage, session string) {
	w.Header().Set("Content-Type", "application/json")
	if session != "" {
		w.Header().Set("Mcp-Session-Id", session)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *AWSMCP) decision(r *http.Request, method, tool, profile string, allow bool, reason string) {
	if a.Log == nil {
		return
	}
	a.Log.Info("decision", "provider", "aws", "op", "aws.mcp", "method", method, "tool", tool,
		"aws_profile", profile, "allow", allow, "reason", reason)
}
