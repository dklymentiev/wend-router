package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Traffic log: when on, every request and response is written as it was,
// one JSON line per event, to logs/traffic-YYYY-MM-DD.jsonl. Events of one
// request share an id: client_request, then upstream_request and
// upstream_response for each model tried, then client_response. Keys are
// never written (no request headers are logged). Off by default: prompts
// and answers end up on disk.

const trafficBodyCap = 8 << 20 // bytes kept per body

type trafficLog struct {
	on   atomic.Bool
	dir  string
	days int

	mu  sync.Mutex
	day string
	f   *os.File
}

func newTrafficLog() *trafficLog {
	t := &trafficLog{dir: os.Getenv("ROUTER_TRAFFIC_DIR"), days: envInt("ROUTER_TRAFFIC_DAYS", 7)}
	if t.dir == "" {
		t.dir = "logs"
		if exe, err := os.Executable(); err == nil {
			t.dir = filepath.Join(filepath.Dir(exe), "logs")
		}
	}
	t.on.Store(os.Getenv("ROUTER_TRAFFIC_LOG") == "1")
	return t
}

// write adds one event. A body that is valid JSON is kept as JSON, anything
// else (an SSE stream, an error page) as text.
func (t *trafficLog) write(ev map[string]any) {
	if t == nil || !t.on.Load() {
		return
	}
	ev["t"] = time.Now().Format(time.RFC3339Nano)
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	d := day(time.Now())
	if d != t.day || t.f == nil {
		if t.f != nil {
			t.f.Close()
		}
		if err := os.MkdirAll(t.dir, 0o700); err != nil {
			log.Printf("traffic log: %v", err)
			return
		}
		f, err := os.OpenFile(filepath.Join(t.dir, "traffic-"+d+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			log.Printf("traffic log: %v", err)
			return
		}
		t.f, t.day = f, d
		t.prune()
	}
	_, _ = t.f.Write(append(line, '\n'))
}

// prune removes log files older than the kept days.
func (t *trafficLog) prune() {
	files, _ := filepath.Glob(filepath.Join(t.dir, "traffic-*.jsonl"))
	sort.Strings(files)
	cutoff := "traffic-" + day(time.Now().AddDate(0, 0, -t.days)) + ".jsonl"
	for _, f := range files {
		if filepath.Base(f) < cutoff {
			_ = os.Remove(f)
		}
	}
}

func jsonOrText(b []byte) any {
	if len(b) > trafficBodyCap {
		return string(b[:trafficBodyCap]) + "...[cut]"
	}
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	return string(b)
}

func newRequestID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tapBody copies what is read from an upstream response and logs it when
// the body is closed.
type tapBody struct {
	io.ReadCloser
	buf   bytes.Buffer
	once  sync.Once
	log   *trafficLog
	event map[string]any
	start time.Time
}

func (b *tapBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.buf.Len() < trafficBodyCap {
		b.buf.Write(p[:n])
	}
	if err != nil && err != io.EOF {
		b.event["read_error"] = err.Error()
	}
	return n, err
}

func (b *tapBody) Close() error {
	b.once.Do(func() {
		b.event["body"] = jsonOrText(b.buf.Bytes())
		b.event["ms"] = time.Since(b.start).Milliseconds()
		b.log.write(b.event)
	})
	return b.ReadCloser.Close()
}

// logUpstream records the request sent to a model and arranges for the
// response to be recorded as it is read.
func (r *Router) logUpstream(ri *reqInfo, m *Model, url string, payload []byte, resp *http.Response, err error, start time.Time) {
	t := r.traffic
	if !t.on.Load() {
		return
	}
	id := ri.id
	t.write(map[string]any{"id": id, "kind": "upstream_request", "model": m.Key, "url": url, "probe": ri.probe, "body": jsonOrText(payload)})
	ev := map[string]any{"id": id, "kind": "upstream_response", "model": m.Key}
	if err != nil {
		ev["error"] = err.Error()
		ev["ms"] = time.Since(start).Milliseconds()
		t.write(ev)
		return
	}
	ev["status"] = resp.StatusCode
	h := map[string]string{}
	for k, v := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || lk == "retry-after" || strings.HasPrefix(lk, "x-ratelimit") || strings.HasPrefix(lk, "ratelimit") || lk == "x-request-id" {
			h[lk] = strings.Join(v, ", ")
		}
	}
	ev["headers"] = h
	resp.Body = &tapBody{ReadCloser: resp.Body, log: t, event: ev, start: start}
}

// recorder keeps what the router wrote back to the client.
type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (w *recorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	if w.buf.Len() < trafficBodyCap {
		w.buf.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *recorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *Router) handleTraffic(w http.ResponseWriter, req *http.Request) {
	if req.Method == "POST" {
		var body struct {
			On bool `json:"on"`
		}
		if err := readJSON(req, &body); err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error")
			return
		}
		r.traffic.on.Store(body.On)
		log.Printf("traffic log %s (%s)", map[bool]string{true: "on", false: "off"}[body.On], r.traffic.dir)
	}
	writeJSON(w, map[string]any{"on": r.traffic.on.Load(), "dir": r.traffic.dir, "days": r.traffic.days})
}
