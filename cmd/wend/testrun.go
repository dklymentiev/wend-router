package main

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"
)

// testRun is a check or a test started from the dashboard (kind "check": one
// short call per model; "test": the test cases): which models it covers,
// how far it got, which models are being asked right now, and a way to stop
// it. The dashboard shows it from the status stream; the background test loop
// has no run.
type testRun struct {
	mu       sync.Mutex
	cancel   context.CancelFunc
	kind     string
	per      int // calls per model
	started  time.Time
	ended    time.Time
	stopped  bool
	total    int
	done     map[*Model]bool
	calls    map[*Model]int // test cases each model has been through so far
	now      map[*Model]bool
	stopM    map[*Model]context.CancelFunc // stops one model's test, the run goes on
	finished bool
}

type testRunView struct {
	Kind      string   `json:"kind"`
	Total     int      `json:"total"`
	Done      int      `json:"done"`       // models finished
	Calls     int      `json:"calls"`      // test cases in the whole run
	CallsDone int      `json:"calls_done"` // of them, gone through
	Now       []string `json:"now"`        // models being tested at this moment
	Running   bool     `json:"running"`
	Stopped   bool     `json:"stopped"`
	Started   int64    `json:"started"`
}

func (t *testRun) view() testRunView {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := testRunView{Kind: t.kind, Total: t.total, Done: len(t.done), Calls: t.total * t.per,
		Running: !t.finished, Stopped: t.stopped, Started: t.started.UnixMilli()}
	for m, n := range t.calls {
		if t.done[m] {
			n = t.per
		}
		v.CallsDone += min(n, t.per)
	}
	for m := range t.now {
		v.Now = append(v.Now, m.Key)
	}
	sort.Strings(v.Now)
	return v
}

// The run of the model, if it is part of the dashboard's run in progress.
func (r *Router) runOf(m *Model) *testRun {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if t := r.run; t != nil && !t.finished {
		t.mu.Lock()
		defer t.mu.Unlock()
		if _, in := t.calls[m]; in {
			return t
		}
	}
	return nil
}

// tick: m went through one more test case.
func (r *Router) tickRun(m *Model) {
	if t := r.runOf(m); t != nil {
		t.mu.Lock()
		t.calls[m]++
		t.mu.Unlock()
	}
}

// runStarts marks m as being tested and gives it its own context, so the
// dashboard can stop this model alone.
func (r *Router) runStarts(ctx context.Context, m *Model) (context.Context, context.CancelFunc) {
	t := r.runOf(m)
	if t == nil {
		return ctx, func() {}
	}
	mctx, cancel := context.WithCancel(ctx)
	t.mu.Lock()
	t.now[m], t.stopM[m] = true, cancel
	t.mu.Unlock()
	return mctx, cancel
}

func (r *Router) runEnds(m *Model, ctx context.Context) {
	if t := r.runOf(m); t != nil {
		t.mu.Lock()
		delete(t.now, m)
		if ctx.Err() == nil { // a stopped model is not done
			t.done[m] = true
		}
		t.mu.Unlock()
	}
}

// runView is what the status stream carries: the run in progress, or the one
// that ended in the last minute, so the dashboard can say how it ended.
func (r *Router) runView() *testRunView {
	r.runMu.Lock()
	t := r.run
	r.runMu.Unlock()
	if t == nil {
		return nil
	}
	t.mu.Lock()
	old := t.finished && time.Since(t.ended) > time.Minute
	t.mu.Unlock()
	if old {
		return nil
	}
	v := t.view()
	return &v
}

// startRun begins a dashboard check or test over the models, unless one is going.
func (r *Router) startRun(kind string, list []*Model) bool {
	r.runMu.Lock()
	if r.run != nil {
		r.run.mu.Lock()
		going := !r.run.finished
		r.run.mu.Unlock()
		if going {
			r.runMu.Unlock()
			return false
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	per, work := len(testCases), r.testAll
	if kind == "check" {
		per, work = 1, r.probeAll
	}
	t := &testRun{cancel: cancel, kind: kind, per: per, started: time.Now(), total: len(list),
		done: map[*Model]bool{}, calls: map[*Model]int{}, now: map[*Model]bool{}, stopM: map[*Model]context.CancelFunc{}}
	for _, m := range list {
		t.calls[m] = 0
	}
	r.run = t
	r.runMu.Unlock()
	go func() {
		work(ctx, list)
		t.mu.Lock()
		t.finished, t.ended = true, time.Now()
		t.mu.Unlock()
		cancel()
	}()
	return true
}

// handleTestStop stops the run, or with ?model= that model's test alone.
func (r *Router) handleTestStop(w http.ResponseWriter, req *http.Request) {
	r.runMu.Lock()
	t := r.run
	r.runMu.Unlock()
	if t == nil {
		writeError(w, 404, "no test run", "invalid_request_error")
		return
	}
	if key := req.URL.Query().Get("model"); key != "" {
		t.mu.Lock()
		var stop context.CancelFunc
		for m, c := range t.stopM {
			if m.Key == key && t.now[m] {
				stop = c
			}
		}
		t.mu.Unlock()
		if stop == nil {
			writeError(w, 404, "that model is not being tested", "invalid_request_error")
			return
		}
		stop()
		writeJSON(w, map[string]any{"stopped": key})
		return
	}
	t.mu.Lock()
	if !t.finished {
		t.stopped = true
	}
	t.mu.Unlock()
	t.cancel()
	writeJSON(w, map[string]any{"stopped": true})
}
