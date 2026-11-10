package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// The pipe: a window into the raw traffic, for the dashboard's Pipe tab.
// What a client sent, what went to each model, the model's stream as it
// arrives and what went back, published live to whoever has the tab open.
// The latest frames (pipeKeep bytes) are kept in memory only, so a tab opened
// after the traffic still shows it; a restart forgets them. Independent of the
// traffic log on disk.
//
// Each frame costs a copy; a viewer that falls behind loses frames, a request
// never waits for it. Everything that leaves for a model is
// shown, the router's own checks and tests too, marked with what sent them:
// the point is to see all that goes out.
// Only a browser on this machine may watch: prompts and answers go through
// it unmasked.

const (
	pipeTextCap  = 256 << 10 // bytes of one request body shown
	pipeChunkCap = 16 << 10  // bytes of one stream chunk shown
	pipeKeep     = 8 << 20   // bytes of the latest frames kept for a tab opened later
)

type pipeEvent struct {
	N      int64  `json:"n"` // frame number: a viewer that reconnects skips what it has
	ID     string `json:"id"`
	T      int64  `json:"t"` // unix ms
	Kind   string `json:"kind"`
	Model  string `json:"model,omitempty"`
	Label  string `json:"label,omitempty"` // "check" or "test: <case>" when the router itself sent it
	Status int    `json:"status,omitempty"`
	Ms     int64  `json:"ms,omitempty"`
	Error  string `json:"error,omitempty"`
	Text   string `json:"text,omitempty"`
	Cut    int    `json:"cut,omitempty"` // bytes left out of Text
}

type pipeHub struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	n      int64
	recent [][]byte // the latest frames, oldest first, pipeKeep bytes at most
	size   int
}

func newPipeHub() *pipeHub { return &pipeHub{subs: map[chan []byte]struct{}{}} }

// on: the pipe keeps the latest traffic whether or not anyone watches.
func (p *pipeHub) on() bool { return p != nil }

func (p *pipeHub) send(e pipeEvent, text []byte, limit int) {
	if !p.on() {
		return
	}
	if len(text) > limit {
		e.Cut = len(text) - limit
		text = text[:limit]
	}
	e.Text = string(text)
	e.T = time.Now().UnixMilli()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	e.N = p.n
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	msg := []byte("data: " + string(data) + "\n\n")
	p.recent = append(p.recent, msg)
	p.size += len(msg)
	for p.size > pipeKeep && len(p.recent) > 1 {
		p.size -= len(p.recent[0])
		p.recent = p.recent[1:]
	}
	for ch := range p.subs {
		select {
		case ch <- msg:
		default: // a slow viewer misses a frame; the request does not wait
		}
	}
}

// subscribe returns the kept frames and a channel for what comes next; taken
// together under the lock, so nothing falls between them.
func (p *pipeHub) subscribe() ([][]byte, chan []byte) {
	ch := make(chan []byte, 512)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subs[ch] = struct{}{}
	return append([][]byte(nil), p.recent...), ch
}

func (p *pipeHub) unsubscribe(ch chan []byte) {
	p.mu.Lock()
	delete(p.subs, ch)
	p.mu.Unlock()
}

// handlePipe streams the pipe to one viewer on this machine.
func (r *Router) handlePipe(w http.ResponseWriter, req *http.Request) {
	// ROUTER_REMOTE_PIPE=1 lets a router that is reached over the network (behind
	// its own access control) show the pipe to the browsers that reach it
	if !fromThisMachine(req) && os.Getenv("ROUTER_REMOTE_PIPE") != "1" {
		writeError(w, 403, "the pipe is shown on this machine only (ROUTER_REMOTE_PIPE=1 allows more)", "forbidden")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	kept, ch := r.pipe.subscribe()
	defer r.pipe.unsubscribe(ch)
	fmt.Fprint(w, ": watching\n\n")
	for _, msg := range kept {
		_, _ = w.Write(msg)
	}
	flusher.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-req.Context().Done():
			return
		case msg := <-ch:
			_, _ = w.Write(msg)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// fromThisMachine: the peer is a loopback address and the page was loaded
// as localhost (a page from elsewhere that rebinds its name to 127.0.0.1
// still carries its own Host).
func fromThisMachine(req *http.Request) bool {
	peer, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(peer); ip == nil || !ip.IsLoopback() {
		return false
	}
	host, _, err := net.SplitHostPort(req.Host)
	if err != nil {
		host = req.Host
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}

// pipeBody shows an upstream response as it is read, then its end.
type pipeBody struct {
	io.ReadCloser
	pipe  *pipeHub
	id    string
	model string
	start time.Time
	once  sync.Once
}

func (b *pipeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.pipe.send(pipeEvent{ID: b.id, Kind: "upstream_chunk", Model: b.model}, p[:n], pipeChunkCap)
	}
	return n, err
}

func (b *pipeBody) Close() error {
	b.once.Do(func() {
		b.pipe.send(pipeEvent{ID: b.id, Kind: "upstream_end", Model: b.model, Ms: time.Since(b.start).Milliseconds()}, nil, 0)
	})
	return b.ReadCloser.Close()
}

// pipeUpstream shows a request sent to a model and taps its response.
func (r *Router) pipeUpstream(ri *reqInfo, m *Model, payload []byte, resp *http.Response, err error, start time.Time) {
	if !r.pipe.on() {
		return
	}
	r.pipe.send(pipeEvent{ID: ri.id, Kind: "upstream_request", Model: m.Key, Label: ri.label}, payload, pipeTextCap)
	if err != nil {
		r.pipe.send(pipeEvent{ID: ri.id, Kind: "upstream_end", Model: m.Key, Error: err.Error(), Ms: time.Since(start).Milliseconds()}, nil, 0)
		return
	}
	r.pipe.send(pipeEvent{ID: ri.id, Kind: "upstream_status", Model: m.Key, Status: resp.StatusCode}, nil, 0)
	resp.Body = &pipeBody{ReadCloser: resp.Body, pipe: r.pipe, id: ri.id, model: m.Key, start: start}
}

// pipeWriter shows what goes back to the client as it is written.
type pipeWriter struct {
	http.ResponseWriter
	pipe   *pipeHub
	id     string
	status int
}

func (w *pipeWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *pipeWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	w.pipe.send(pipeEvent{ID: w.id, Kind: "client_chunk"}, p, pipeChunkCap)
	return w.ResponseWriter.Write(p)
}

func (w *pipeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
