package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// The prober sends a small real request to models that have not been
// measured, or have been idle a while, so the router knows which models
// work and how fast before it needs them. One probe per provider at a
// time; paid models are never probed.

const (
	probeTimeout = 90 * time.Second
	probeTokens  = 200
	probePrompt  = "Count from 1 to 20, separated by spaces. Reply with the numbers only."
)

func (r *Router) probeEvery(m *Model) time.Duration {
	if d, err := time.ParseDuration(m.Provider.Spec.ProbeEvery); err == nil && d > 0 {
		return d
	}
	return r.cfg.ProbeEvery
}

// needsProbe says whether m is due: never measured, or idle past its interval.
func (r *Router) needsProbe(m *Model, now time.Time) bool {
	if m.paid() || !m.usable(now) {
		return false
	}
	m.mu.Lock()
	last := latest(m.lastOK, m.lastProbe, m.lastErrAt)
	measured := m.ttftMs > 0 || m.ok > 0
	m.mu.Unlock()
	if !measured && m.lastProbe.IsZero() {
		return true
	}
	every := r.probeEvery(m)
	if m.health() == healthFailing {
		every /= 4 // win it back sooner, still sparing scarce quotas
	}
	return now.Sub(last) > every
}

func latest(ts ...time.Time) time.Time {
	var t time.Time
	for _, x := range ts {
		if x.After(t) {
			t = x
		}
	}
	return t
}

// probe sends one small request to m and records the result as if it were
// traffic: speed, health, quota, refusals.
func (r *Router) probe(ctx context.Context, m *Model) {
	m.mu.Lock()
	m.lastProbe = time.Now()
	m.mu.Unlock()
	body := map[string]json.RawMessage{
		"messages":   json.RawMessage(`[{"role":"user","content":"` + probePrompt + `"}]`),
		"max_tokens": json.RawMessage("200"),
	}
	ri := &reqInfo{model: m.Key, body: body, promptTokens: 30, outTokens: probeTokens, estTokens: 30 + probeTokens, probe: true, id: "probe-" + newRequestID(), label: "check"}
	req, _ := http.NewRequestWithContext(ctx, "POST", "/probe", nil)
	tried := []string{m.Key}

	m.begin()
	defer m.end()
	u, ae := r.openStream(req, ri, m, probeTimeout)
	if ae == nil {
		ae = r.drain(req, ri, m, u)
	}
	if ae != nil {
		if ae.clientGone {
			return
		}
		if ae.kind == failAuth {
			m.Provider.refuseKey(truncate(ae.msg, 120))
		}
		m.recordFailure(ae.kind, ae.pause, ae.msg, ri)
		r.logProbe(ri, m.Key, "failed", tried, []string{m.Key + " (" + ae.kind.String() + ")"})
		return
	}
	r.settle(m, ri)
	r.logProbe(ri, m.Key, "ok", tried, nil)
}

// drain reads a probe's stream to the end and records its speed.
func (r *Router) drain(req *http.Request, ri *reqInfo, m *Model, u *upstream) *attemptErr {
	defer u.close()
	if u.whole != nil {
		ri.tokens = completionTokens(u.whole)
		ri.takeUsage(readUsage(u.whole))
	} else {
		var a assembler
		for _, l := range u.held {
			a.line(l)
		}
		for {
			line, err := u.br.ReadBytes('\n')
			if len(line) > 0 {
				if ae := a.line(line); ae != nil {
					return ae
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return netFailure(req, err)
			}
		}
		if a.empty() {
			return &attemptErr{kind: failEmpty, msg: "the model answered with nothing"}
		}
		ri.tokens = a.tokens()
		ri.takeUsage(readUsage(a.usage))
		if a.reasoning.Len() > 0 {
			m.sawReasoning()
		}
	}
	ri.ttft, ri.total = u.ttft, time.Since(u.start)
	m.recordSuccess(ri.ttft, ri.total, ri.tokens)
	return nil
}

func (r *Router) logProbe(ri *reqInfo, model, outcome string, tried, failures []string) {
	ri.probe = true
	r.logRequest(ri, model, outcome, tried, failures)
}

// probeAll probes the given models, one at a time per provider.
func (r *Router) probeAll(ctx context.Context, models []*Model) {
	byProvider := map[*Provider][]*Model{}
	for _, m := range models {
		byProvider[m.Provider] = append(byProvider[m.Provider], m)
	}
	var wg sync.WaitGroup
	for _, list := range byProvider {
		wg.Go(func() {
			for _, m := range list {
				if ctx.Err() != nil {
					return
				}
				mctx, stop := r.runStarts(ctx, m)
				r.probe(mctx, m)
				r.runEnds(m, mctx)
				stop()
			}
		})
	}
	wg.Wait()
}

// ProbeLoop probes what is due now, then checks again every minute.
func (r *Router) ProbeLoop(ctx context.Context) {
	run := func() {
		now := time.Now()
		r.mu.RLock()
		var due []*Model
		for _, m := range r.models {
			if r.needsProbe(m, now) {
				due = append(due, m)
			}
		}
		r.mu.RUnlock()
		if len(due) > 0 {
			log.Printf("probing %d models", len(due))
			r.probeAll(ctx, due)
		}
	}
	run()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// handleProbe probes one model (?model=provider/id) or every free model, in
// the background; results arrive on the dashboard as they come.
func (r *Router) handleProbe(w http.ResponseWriter, req *http.Request) {
	key := req.URL.Query().Get("model")
	now := time.Now()
	r.mu.RLock()
	var list []*Model
	for _, m := range r.models {
		if (key == "" || m.Key == key) && !m.paid() && (key != "" || m.usable(now)) {
			list = append(list, m)
		}
	}
	r.mu.RUnlock()
	if len(list) == 0 {
		writeError(w, 404, "no such free model", "invalid_request_error")
		return
	}
	if !r.startRun("check", list) {
		writeError(w, 409, "a check or test is already running; stop it or wait", "invalid_request_error")
		return
	}
	writeJSON(w, map[string]any{"probing": len(list)})
}
