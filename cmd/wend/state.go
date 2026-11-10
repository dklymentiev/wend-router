package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// The router remembers what it learned across restarts: model speeds and
// health, refused and paused models, learned capabilities, the spend ledger
// and the recent requests. Without it a restart sends the first requests to
// models already known to fail, and resets the daily budget.

const stateVersion = 1

type modelState struct {
	TTFTMs      float64           `json:"ttft_ms,omitempty"`
	TotalMs     float64           `json:"total_ms,omitempty"`
	TPS         float64           `json:"tps,omitempty"`
	TPSv        int               `json:"tps_v,omitempty"`        // 2: measured over at least minGenWindow
	CapWindowS  int               `json:"cap_window_s,omitempty"` // Model.capWindow
	OK          int               `json:"ok,omitempty"`
	Errors      int               `json:"errors,omitempty"`
	History     []bool            `json:"history,omitempty"` // oldest first
	Context     int               `json:"context,omitempty"`
	Tools       capability        `json:"tools,omitempty"`
	Vision      capability        `json:"vision,omitempty"`
	Reasoning   capability        `json:"reasoning,omitempty"`
	GoneUntil   time.Time         `json:"gone_until,omitzero"`
	PausedUntil time.Time         `json:"paused_until,omitzero"`
	OpenUntil   time.Time         `json:"open_until,omitzero"`
	Trips       int               `json:"trips,omitempty"`
	LastError   string            `json:"last_error,omitempty"`
	LastErrorAt time.Time         `json:"last_error_at,omitzero"`
	LastOK      time.Time         `json:"last_ok,omitzero"`
	LastProbe   time.Time         `json:"last_probe,omitzero"`
	Usually     repData           `json:"usually"`
	Test        *testResult       `json:"test,omitempty"`
	TestPart    *testResult       `json:"test_part,omitempty"`
	Windows     map[string]window `json:"windows,omitempty"`
	MaxRequest  int               `json:"max_request,omitempty"`
	DropNames   bool              `json:"drop_names,omitempty"`
	TopP1       bool              `json:"top_p_1,omitempty"`
}

type routerState struct {
	Providers map[string]repData                `json:"providers,omitempty"`
	Version   int                               `json:"version"`
	SavedAt   time.Time                         `json:"saved_at"`
	Models    map[string]modelState             `json:"models"`
	Spend     map[string]*dayLedger             `json:"spend"`
	Log       []requestLog                      `json:"log,omitempty"`
	TrafficOn bool                              `json:"traffic_on,omitempty"`
	Usage     map[string]map[string]*usageCount `json:"usage,omitempty"`
	Asked     map[string]time.Time              `json:"asked,omitempty"` // when each model name was last asked for
}

func (m *Model) saveState() modelState {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := modelState{
		TTFTMs: m.ttftMs, TotalMs: m.totalMs, TPS: m.tps, TPSv: 2, CapWindowS: int(m.capWindow.Seconds()), OK: m.ok, Errors: m.errs,
		Context: m.context, Tools: m.tools, Vision: m.vision, Reasoning: m.reasoning,
		GoneUntil: m.goneUntil, PausedUntil: m.pausedUntil, OpenUntil: m.openUntil, Trips: m.trips,
		LastError: m.lastErr, LastErrorAt: m.lastErrAt, LastOK: m.lastOK, LastProbe: m.lastProbe,
	}
	s.Usually = m.rep.snapshot()
	s.Test = m.test
	s.MaxRequest, s.DropNames, s.TopP1 = m.maxRequest, m.dropNames, m.topP1
	s.TestPart = m.testPart
	s.Windows = m.windows
	n := min(m.histN, historySize)
	start := 0
	if m.histN > historySize {
		start = m.histN % historySize
	}
	for i := range n {
		s.History = append(s.History, m.history[(start+i)%historySize])
	}
	return s
}

// loadState lays saved knowledge over a freshly discovered model. A saved
// capability wins only when it says something: a learned "no" outlives a
// list that does not mention the capability.
func (m *Model) loadState(s modelState) {
	m.rep.restore(s.Usually)
	m.mu.Lock()
	m.windows = s.Windows
	m.maxRequest, m.dropNames, m.topP1 = s.MaxRequest, s.DropNames, s.TopP1
	if s.TestPart != nil && s.TestPart.Version == testVersion {
		m.testPart = s.TestPart
	}
	if s.Test != nil && s.Test.Version == testVersion {
		m.test = s.Test
		m.Tier = s.Test.tier()
	}
	defer m.mu.Unlock()
	m.ttftMs, m.totalMs, m.ok, m.errs = s.TTFTMs, s.TotalMs, s.OK, s.Errors
	if s.TPSv >= 2 {
		m.tps = s.TPS
	}
	m.capWindow = time.Duration(s.CapWindowS) * time.Second
	if s.Context > 0 && (m.context == 0 || s.Context < m.context) {
		m.context = s.Context
	}
	for _, c := range []struct {
		dst *capability
		src capability
	}{{&m.tools, s.Tools}, {&m.vision, s.Vision}, {&m.reasoning, s.Reasoning}} {
		if c.src != capUnknown {
			*c.dst = c.src
		}
	}
	m.goneUntil, m.pausedUntil, m.openUntil, m.trips = s.GoneUntil, s.PausedUntil, s.OpenUntil, s.Trips
	m.lastErr, m.lastErrAt, m.lastOK, m.lastProbe = s.LastError, s.LastErrorAt, s.LastOK, s.LastProbe
	m.histN = 0
	for _, ok := range s.History {
		m.push(ok)
	}
}

func statePath() string {
	if p := os.Getenv("ROUTER_STATE"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "router-state.json")
	}
	return "router-state.json"
}

// LoadState reads the saved state. Call it before the first Discover: the
// model part is applied as models are discovered.
func (r *Router) LoadState() error {
	b, err := os.ReadFile(statePath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var s routerState
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s.Version != stateVersion {
		log.Printf("state file version %d, expected %d: starting fresh", s.Version, stateVersion)
		return nil
	}
	r.mu.Lock()
	r.pending = s.Models
	r.pendingProviders = s.Providers
	for _, p := range r.providers {
		if rep, ok := s.Providers[p.Spec.ID]; ok {
			p.rep.restore(rep)
		}
	}
	r.mu.Unlock()
	if s.Spend != nil {
		r.spent.restore(s.Spend)
	}
	r.usage.restore(s.Usage)
	r.usage.restoreAsked(s.Asked)
	if _, set := os.LookupEnv("ROUTER_TRAFFIC_LOG"); !set {
		r.traffic.on.Store(s.TrafficOn) // the dashboard switch outlives a restart
	}
	h := r.live
	h.mu.Lock()
	if len(s.Log) > logSize {
		s.Log = s.Log[len(s.Log)-logSize:]
	}
	h.log = s.Log
	h.next = len(s.Log) % logSize
	h.mu.Unlock()
	return nil
}

// SaveState writes the state atomically: a crash midway leaves the old file.
func (r *Router) SaveState() error {
	s := routerState{Version: stateVersion, SavedAt: time.Now(), Models: map[string]modelState{}}
	r.mu.RLock()
	for k, v := range r.pending { // known models not discovered this run
		s.Models[k] = v
	}
	for _, m := range r.models {
		s.Models[m.Key] = m.saveState()
	}
	s.Providers = map[string]repData{}
	for k, v := range r.pendingProviders {
		s.Providers[k] = v
	}
	for _, p := range r.providers {
		s.Providers[p.Spec.ID] = p.rep.snapshot()
	}
	r.mu.RUnlock()
	s.Spend = r.spent.snapshot()
	s.Log = r.live.recent()
	s.TrafficOn = r.traffic.on.Load()
	s.Usage = r.usage.snapshot()
	s.Asked = r.usage.askedSnapshot()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath())
}

// StateLoop saves the state every interval until stop is closed, then once more.
func (r *Router) StateLoop(stop <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			if err := r.SaveState(); err != nil {
				log.Printf("state: %v", err)
			}
			return
		case <-t.C:
			if err := r.SaveState(); err != nil {
				log.Printf("state: %v", err)
			}
		}
	}
}
