package main

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Paid models are off unless a cost cap (ROUTER_MAX_COST or a profile's
// max_cost) is above zero. Then a paid model may take a request whose
// estimated cost is within the cap, after every free model failed, and
// only while today's spend is under the daily budget.

const (
	defaultOutTokens = 1000 // output assumed for the estimate when max_tokens is not set
	ledgerDays       = 400  // days of spend kept
)

// dayLedger is one day of money spent through the router.
type dayLedger struct {
	Total    float64            `json:"total"`
	Paid     int                `json:"paid_requests"`
	ByModel  map[string]float64 `json:"by_model,omitempty"`
	Surprise float64            `json:"surprise,omitempty"` // charged by models the router took for free
}

// ledger keeps spend per day; it is saved with the router state.
type ledger struct {
	mu   sync.Mutex
	days map[string]*dayLedger
}

func day(t time.Time) string { return t.Format("2006-01-02") }

func (l *ledger) add(model string, usd float64, surprise bool) {
	if usd <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.days == nil {
		l.days = map[string]*dayLedger{}
	}
	k := day(time.Now())
	d := l.days[k]
	if d == nil {
		d = &dayLedger{ByModel: map[string]float64{}}
		l.days[k] = d
	}
	d.Total += usd
	d.Paid++
	d.ByModel[model] += usd
	if surprise {
		d.Surprise += usd
	}
}

// sum is the spend over the last n days, today included; n <= 0 is all time.
func (l *ledger) sum(n int) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	from := ""
	if n > 0 {
		from = day(time.Now().AddDate(0, 0, -(n - 1)))
	}
	t := 0.0
	for k, d := range l.days {
		if k >= from {
			t += d.Total
		}
	}
	return t
}

func (l *ledger) today() float64 { return l.sum(1) }

func (l *ledger) surpriseToday() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if d := l.days[day(time.Now())]; d != nil {
		return d.Surprise
	}
	return 0
}

func (l *ledger) snapshot() map[string]*dayLedger {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := make([]string, 0, len(l.days))
	for k := range l.days {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	out := map[string]*dayLedger{}
	for i, k := range keys {
		if i >= ledgerDays {
			delete(l.days, k)
			continue
		}
		d := *l.days[k]
		d.ByModel = make(map[string]float64, len(l.days[k].ByModel))
		for m, v := range l.days[k].ByModel {
			d.ByModel[m] = v
		}
		out[k] = &d
	}
	return out
}

func (l *ledger) restore(days map[string]*dayLedger) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.days = days
	for _, d := range l.days {
		if d.ByModel == nil {
			d.ByModel = map[string]float64{}
		}
	}
}

func (m *Model) paid() bool {
	return m.priceIn > 0 || m.priceOut > 0
}

// estimateCost is what the request would cost on m, in USD.
func (m *Model) estimateCost(ri *reqInfo) float64 {
	return float64(ri.promptTokens)*m.priceIn + float64(ri.outTokens)*m.priceOut
}

// costOf is what a served request cost, from the tokens actually used.
func (m *Model) costOf(prompt, completion int) float64 {
	return float64(prompt)*m.priceIn + float64(completion)*m.priceOut
}

// affordable reports whether m may serve the request under the cost cap
// and today's budget. Free models always may.
func (r *Router) affordable(m *Model, ri *reqInfo, cap float64) bool {
	if !m.paid() {
		return true
	}
	if cap <= 0 || r.spent.today() >= r.cfg.DailyBudget {
		return false
	}
	return m.estimateCost(ri) <= cap
}

// settle works out what a served request cost and books it. The provider's
// own figure (OpenRouter's usage.cost) wins over the estimate from tokens.
func (r *Router) settle(m *Model, ri *reqInfo) {
	switch {
	case ri.reportedCost != nil:
		ri.cost, ri.costSource = *ri.reportedCost, "provider"
	case m.paid():
		ri.cost, ri.costSource = m.costOf(ri.promptTokens, ri.tokens), "estimate"
	default:
		ri.cost, ri.costSource = 0, "free"
	}
	r.spent.add(m.Key, ri.cost, ri.cost > 0 && !m.paid())
}

// usageInfo is what a response says about its own size and price.
type usageInfo struct {
	prompt, completion int
	cost               *float64 // USD, when the provider reports it
}

// readUsage reads usage from a response or chunk; a bare usage object is
// accepted too.
func readUsage(b []byte) usageInfo {
	type usage struct {
		PromptTokens     int      `json:"prompt_tokens"`
		CompletionTokens int      `json:"completion_tokens"`
		Cost             *float64 `json:"cost"`
	}
	var u struct {
		Usage *usage `json:"usage"`
		usage
	}
	if len(b) == 0 || json.Unmarshal(b, &u) != nil {
		return usageInfo{}
	}
	if u.Usage != nil {
		return usageInfo{u.Usage.PromptTokens, u.Usage.CompletionTokens, u.Usage.Cost}
	}
	return usageInfo{u.PromptTokens, u.CompletionTokens, u.Cost}
}

// completionTokens reads usage.completion_tokens; 0 when absent.
func completionTokens(b []byte) int {
	return readUsage(b).completion
}

// takeUsage copies what a response reported into the request's figures.
func (ri *reqInfo) takeUsage(u usageInfo) {
	if u.prompt > 0 {
		ri.promptTokens = u.prompt
		ri.promptReported = true
	}
	if u.completion > 0 {
		ri.outReported = true
	}
	if u.cost != nil {
		ri.reportedCost = u.cost
	}
}
