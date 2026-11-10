package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

const logSize = 500 // requests kept for the dashboard

// requestLog is one finished request as the dashboard shows it.
type requestLog struct {
	Time     int64    `json:"t"` // unix ms
	Model    string   `json:"model,omitempty"`
	Asked    string   `json:"asked,omitempty"`  // the model name the client sent: a profile or a model
	Client   string   `json:"client,omitempty"` // who sent it (X-Router-Client or the user agent)
	Outcome  string   `json:"outcome"`          // ok, failed, client-error, client-left, broke-midway
	Stream   bool     `json:"stream"`
	TTFTMs   int      `json:"ttft_ms,omitempty"`
	TotalMs  int      `json:"total_ms,omitempty"`
	Tokens   int      `json:"tokens,omitempty"`
	TokensIn int      `json:"tokens_in,omitempty"`
	TPS      float64  `json:"tps,omitempty"`
	Cost     float64  `json:"cost,omitempty"`      // USD
	CostFrom string   `json:"cost_from,omitempty"` // free, provider or estimate
	Probe    bool     `json:"probe,omitempty"`
	Attempts int      `json:"attempts"`
	Failures []string `json:"failures,omitempty"`
}

// liveHub keeps the recent requests and fans out events to dashboards.
type liveHub struct {
	mu   sync.Mutex
	log  []requestLog // ring
	next int
	subs map[chan []byte]struct{}
}

func newLiveHub() *liveHub {
	return &liveHub{subs: map[chan []byte]struct{}{}}
}

func (r *Router) logRequest(ri *reqInfo, model, outcome string, tried, failures []string) {
	r.usage.record(ri, model, outcome, failures)
	e := requestLog{
		Time: time.Now().UnixMilli(), Model: model, Asked: nonEmptyOr(ri.model, "auto"), Client: ri.client, Outcome: outcome, Stream: ri.stream,
		Attempts: len(tried), Failures: failures, Probe: ri.probe,
	}
	if outcome == "ok" {
		e.TTFTMs, e.TotalMs, e.Tokens, e.TokensIn = int(ri.ttft.Milliseconds()), int(ri.total.Milliseconds()), ri.tokens, ri.promptTokens
		e.TPS = float64(int(tokensPerSecond(ri.ttft, ri.total, ri.tokens)*10)) / 10
		e.Cost, e.CostFrom = ri.cost, ri.costSource
	}
	if !ri.probe && ri.session != "" {
		in, out := 0, 0
		if outcome == "ok" {
			in, out = ri.promptTokens, ri.tokens
		}
		r.book.finish(ri.session, model, failures, in, out)
	}
	h := r.live
	h.mu.Lock()
	if len(h.log) < logSize {
		h.log = append(h.log, e)
	} else {
		h.log[h.next] = e
	}
	h.next = (h.next + 1) % logSize
	h.mu.Unlock()
	h.publish("request", e)
}

// recent returns the logged requests, oldest first.
func (h *liveHub) recent() []requestLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]requestLog, len(h.log))
	copy(out, h.log)
	sort.Slice(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out
}

func (h *liveHub) publish(event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default: // a slow dashboard misses an update rather than stalling requests
		}
	}
}

func (h *liveHub) subscribe() chan []byte {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *liveHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *liveHub) watched() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

// liveStatus is the once-a-second picture: models plus totals.
func (r *Router) liveStatus() map[string]any {
	models := r.snapshot()
	inflight, rpm := 0, 0
	for _, m := range models {
		inflight += m.InFlight
		rpm += m.RPM
	}
	active := ""
	if c := r.candidates(&reqInfo{model: "auto", outTokens: defaultOutTokens}); len(c) > 0 {
		active = c[0].Key
	}
	return map[string]any{
		"time": time.Now().UnixMilli(), "inflight": inflight, "rpm": rpm,
		"active": active, "models": models, "sessions": r.book.active(15 * time.Minute), "clients": r.book.clients(time.Hour),
		"tokens_minute": r.tokensLastMinute(), "test_run": r.runView(),
		"spent": r.spent.today(), "spent_week": r.spent.sum(7), "spent_all": r.spent.sum(0),
		"surprise": r.spent.surpriseToday(), "accounts": r.accounts(),
		"budget": r.cfg.DailyBudget, "max_cost": r.cfg.MaxCost,
	}
}

// StatusLoop publishes the live status every second while a dashboard is open.
func (r *Router) StatusLoop(stop <-chan struct{}) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if r.live.watched() {
				r.live.publish("status", r.liveStatus())
			}
		}
	}
}

func (r *Router) handleEvents(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	ch := r.live.subscribe()
	defer r.live.unsubscribe(ch)

	write := func(event string, v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	}
	write("history", r.live.recent())
	write("status", r.liveStatus())
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

func (r *Router) handleDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(dashboardHTML)
}

// tokensLastMinute is what passed through in the last 60 seconds, agents'
// requests only: input and output tokens.
func (r *Router) tokensLastMinute() map[string]int {
	cut := time.Now().Add(-time.Minute).UnixMilli()
	in, out := 0, 0
	for _, e := range r.live.recent() {
		if e.Time >= cut && !e.Probe && e.Outcome == "ok" {
			in += e.TokensIn
			out += e.Tokens
		}
	}
	return map[string]int{"in": in, "out": out}
}
