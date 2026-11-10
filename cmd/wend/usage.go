package main

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The usage ledger: requests and tokens per day, model and client, kept in
// the state file so a restart loses nothing. Tokens are the provider's
// count where it reports one and the router's estimate otherwise; the
// estimated part is kept apart so the totals say how sure they are.

const usageDays = 60 // days kept

type usageCount struct {
	Requests     int `json:"requests"`
	OK           int `json:"ok"`
	Failed       int `json:"failed"`
	TokensIn     int `json:"tokens_in"`
	TokensOut    int `json:"tokens_out"`
	EstimatedIn  int `json:"estimated_in,omitempty"`  // part of tokens_in the router estimated
	EstimatedOut int `json:"estimated_out,omitempty"` // part of tokens_out the router estimated
}

func (c *usageCount) add(o *usageCount) {
	c.Requests += o.Requests
	c.OK += o.OK
	c.Failed += o.Failed
	c.TokensIn += o.TokensIn
	c.TokensOut += o.TokensOut
	c.EstimatedIn += o.EstimatedIn
	c.EstimatedOut += o.EstimatedOut
}

// usageLedger maps a day ("2006-01-02", local time) to counts keyed by
// "provider/model|client".
type usageLedger struct {
	mu   sync.Mutex
	days map[string]map[string]*usageCount
	// asked: when a client last sent each model name (a profile or a model),
	// probes left out; the dashboard shows which profiles are in use.
	asked map[string]time.Time
}

func newUsageLedger() *usageLedger {
	return &usageLedger{days: map[string]map[string]*usageCount{}, asked: map[string]time.Time{}}
}

func usageKey(model, client string) string { return model + "|" + client }

func splitUsageKey(k string) (model, client string) {
	model, client, _ = strings.Cut(k, "|")
	return model, client
}

func (u *usageLedger) count(day, model, client string) *usageCount {
	d := u.days[day]
	if d == nil {
		d = map[string]*usageCount{}
		u.days[day] = d
	}
	c := d[usageKey(model, client)]
	if c == nil {
		c = &usageCount{}
		d[usageKey(model, client)] = c
	}
	return c
}

// record books one finished request: the model that answered (if any) and
// every model that failed on the way.
func (u *usageLedger) record(ri *reqInfo, model, outcome string, failures []string) {
	client := nonEmptyOr(ri.client, "unknown")
	if ri.probe {
		client = "router-checks"
	}
	day := time.Now().Format("2006-01-02")
	u.mu.Lock()
	defer u.mu.Unlock()
	if !ri.probe {
		u.asked[nonEmptyOr(ri.model, "auto")] = time.Now()
	}
	for _, f := range failures {
		key, _, _ := strings.Cut(f, " (")
		c := u.count(day, key, client)
		c.Requests++
		c.Failed++
	}
	if model == "" || outcome != "ok" {
		return
	}
	c := u.count(day, model, client)
	c.Requests++
	c.OK++
	c.TokensIn += ri.promptTokens
	c.TokensOut += ri.tokens
	if !ri.promptReported {
		c.EstimatedIn += ri.promptTokens
	}
	if !ri.outReported {
		c.EstimatedOut += ri.tokens
	}
}

func (u *usageLedger) snapshot() map[string]map[string]*usageCount {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]map[string]*usageCount, len(u.days))
	for day, d := range u.days {
		cp := make(map[string]*usageCount, len(d))
		for k, c := range d {
			v := *c
			cp[k] = &v
		}
		out[day] = cp
	}
	return out
}

func (u *usageLedger) askedSnapshot() map[string]time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]time.Time, len(u.asked))
	for k, v := range u.asked {
		out[k] = v
	}
	return out
}

func (u *usageLedger) restoreAsked(asked map[string]time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, v := range asked {
		u.asked[k] = v
	}
}

func (u *usageLedger) restore(days map[string]map[string]*usageCount) {
	u.mu.Lock()
	defer u.mu.Unlock()
	cutoff := time.Now().AddDate(0, 0, -usageDays).Format("2006-01-02")
	for day, d := range days {
		if day >= cutoff && d != nil {
			u.days[day] = d
		}
	}
}

// usageDay is one day of the report.
type usageDay struct {
	Date       string                 `json:"date"`
	Total      usageCount             `json:"total"`
	ByProvider map[string]*usageCount `json:"by_provider"`
	ByClient   map[string]*usageCount `json:"by_client"`
	ByModel    map[string]*usageCount `json:"by_model"`
}

// report sums the last n days, newest first.
func (u *usageLedger) report(n int) []usageDay {
	snap := u.snapshot()
	var dates []string
	for d := range snap {
		dates = append(dates, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	if len(dates) > n {
		dates = dates[:n]
	}
	out := make([]usageDay, 0, len(dates))
	for _, date := range dates {
		day := usageDay{Date: date, ByProvider: map[string]*usageCount{}, ByClient: map[string]*usageCount{}, ByModel: map[string]*usageCount{}}
		for k, c := range snap[date] {
			model, client := splitUsageKey(k)
			provider, _, _ := strings.Cut(model, "/")
			for _, m := range []struct {
				into map[string]*usageCount
				key  string
			}{{day.ByProvider, provider}, {day.ByClient, client}, {day.ByModel, model}} {
				if m.into[m.key] == nil {
					m.into[m.key] = &usageCount{}
				}
				m.into[m.key].add(c)
			}
			day.Total.add(c)
		}
		out = append(out, day)
	}
	return out
}

// GET /api/usage?days=7
func (r *Router) handleUsage(w http.ResponseWriter, req *http.Request) {
	n, err := strconv.Atoi(req.URL.Query().Get("days"))
	if err != nil || n <= 0 {
		n = 7
	}
	writeJSON(w, map[string]any{"days": r.usage.report(min(n, usageDays)), "asked": r.usage.askedSnapshot()})
}

// An image in a request is a large base64 string but costs the model about
// imageTokens; counting its characters made a request with screenshots look
// tens of times bigger than it is.
const imageTokens = 1500

var inlineImage = regexp.MustCompile(`data:image/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=]+`)

// promptEstimate is the rough size in tokens of the messages and tools.
func promptEstimate(msgs, tools []byte) int {
	images := 0
	text := inlineImage.ReplaceAllFunc(msgs, func([]byte) []byte { images++; return nil })
	return (len(text)+len(tools))/4 + images*imageTokens
}
