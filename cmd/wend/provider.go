package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Provider is one API endpoint with its keys and on/off state.
type Provider struct {
	Spec    ProviderSpec
	BaseURL string

	mu       sync.Mutex
	keys     []providerKey // in order of use
	cur      int           // the key in use
	offUntil time.Time     // every key refused, or the account is empty
	offWhy   string
	listErr  string // why the last model discovery failed, empty when it worked
	account  *accountInfo
	rep      reputation // how the provider usually behaves, across its models

	inflight  atomic.Int32 // requests to any of its models right now
	busyLimit int32        // requests at once it took before refusing, 0 when never refused
	busyUntil time.Time    // the limit is trusted until then, then tried again
}

// busyFor is how long a learned concurrency limit holds before the router
// tries one more again: such limits scale with the account and change.
const busyFor = 30 * time.Minute

// noteBusy records a "too many requests at once" refusal: the provider
// takes one fewer than it had in flight (this request included).
func (p *Provider) noteBusy() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.busyLimit = max(1, p.inflight.Load()-1)
	p.busyUntil = time.Now().Add(busyFor)
}

// hasRoom reports whether one more request fits under the learned limit.
func (p *Provider) hasRoom() bool {
	p.mu.Lock()
	limit, until := p.busyLimit, p.busyUntil
	p.mu.Unlock()
	return limit == 0 || time.Now().After(until) || p.inflight.Load() < limit
}

// providerKey is one key: from env (wend.env) or added in the dashboard.
type providerKey struct {
	ID     string
	Value  string
	Label  string
	Source string // env or dashboard
	BadWhy string // set when the provider refused it
}

const providerOffPeriod = time.Hour

// newProvider returns nil when the provider has no key, unless it works
// without one. Keys come from env first, then the given extra keys.
func newProvider(spec ProviderSpec, getenv func(string) string, extra ...providerKey) *Provider {
	p := &Provider{Spec: spec, BaseURL: strings.TrimRight(spec.BaseURL, "/")}
	if spec.BaseURLEnv != "" {
		if v := strings.TrimSpace(getenv(spec.BaseURLEnv)); v != "" {
			p.BaseURL = strings.TrimRight(v, "/")
			if !strings.HasSuffix(p.BaseURL, "/v1") && spec.NoKey {
				p.BaseURL += "/v1" // OLLAMA_URL is usually given without /v1
			}
		}
	}
	// {NAME} in the URL is filled from env (Cloudflare's account id).
	for _, name := range urlVars(p.BaseURL) {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return nil
		}
		p.BaseURL = strings.ReplaceAll(p.BaseURL, "{"+name+"}", v)
	}
	if spec.NoKey && len(spec.KeyEnv) == 0 && len(extra) == 0 {
		return p // works without a key; one added in the dashboard is used (OVH then bills per use: key_note says so)
	}
	seen := map[string]bool{}
	for _, name := range spec.KeyEnv {
		if v := strings.TrimSpace(getenv(name)); v != "" && !seen[v] {
			seen[v] = true
			p.keys = append(p.keys, providerKey{ID: keyID(v), Value: v, Label: name, Source: "env"})
		}
	}
	for _, k := range extra {
		if !seen[k.Value] {
			seen[k.Value] = true
			p.keys = append(p.keys, k)
		}
	}
	if len(p.keys) == 0 && !spec.NoKey {
		return nil
	}
	return p
}

// urlVars lists the {NAME} placeholders in a URL.
func urlVars(u string) []string {
	var out []string
	for {
		i := strings.Index(u, "{")
		if i < 0 {
			return out
		}
		j := strings.Index(u[i:], "}")
		if j < 0 {
			return out
		}
		out = append(out, u[i+1:i+j])
		u = u[i+j+1:]
	}
}

func (p *Provider) usable(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return now.After(p.offUntil)
}

func (p *Provider) turnOff(why string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.offUntil = time.Now().Add(providerOffPeriod)
	p.offWhy = why
}

// refuseKey marks the key in use as refused and moves to the next good one.
// With no good key left the provider is turned off for a while.
func (p *Provider) refuseKey(why string) {
	p.mu.Lock()
	if len(p.keys) > 0 {
		p.keys[p.cur].BadWhy = why
		for i := 1; i <= len(p.keys); i++ {
			if n := (p.cur + i) % len(p.keys); p.keys[n].BadWhy == "" {
				p.cur = n
				p.mu.Unlock()
				return
			}
		}
	}
	p.mu.Unlock()
	p.turnOff(why)
}

// setKeys replaces the keys, keeping what is known about ones still there,
// and gives the provider another chance.
func (p *Provider) setKeys(keys []providerKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := map[string]providerKey{}
	for _, k := range p.keys {
		old[k.ID] = k
	}
	curID := ""
	if p.cur < len(p.keys) {
		curID = p.keys[p.cur].ID
	}
	p.keys, p.cur = keys, 0
	for i := range p.keys {
		if o, ok := old[p.keys[i].ID]; ok {
			p.keys[i].BadWhy = o.BadWhy
		}
		if p.keys[i].ID == curID && p.keys[i].BadWhy == "" {
			p.cur = i
		}
	}
	if p.cur < len(p.keys) && p.keys[p.cur].BadWhy != "" {
		for i := range p.keys {
			if p.keys[i].BadWhy == "" {
				p.cur = i
				break
			}
		}
	}
	p.offUntil, p.offWhy = time.Time{}, ""
}

func (p *Provider) keyList() []providerKey {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]providerKey(nil), p.keys...)
}

func (p *Provider) authorize(req *http.Request) {
	p.mu.Lock()
	key := ""
	if p.cur < len(p.keys) {
		key = p.keys[p.cur].Value
	}
	p.mu.Unlock()
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// liveModel is what a provider's /models says about one model. Providers
// put the same facts under different names; whatever is found is kept.
type liveModel struct {
	ID          string
	Description string
	Context     int
	Free        bool
	PriceIn     float64 // USD per token, -1 when the provider says the price varies
	PriceOut    float64
	Tools       *bool
	Reasoning   *bool
	Vision      *bool
}

func (p *Provider) listModels(ctx context.Context, client *http.Client) ([]liveModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", p.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	p.authorize(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, &httpError{status: resp.StatusCode, body: truncate(string(body), 300)}
	}
	var raw struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("models list: %w", err)
	}
	out := make([]liveModel, 0, len(raw.Data))
	for _, d := range raw.Data {
		var id string
		_ = json.Unmarshal(d["id"], &id)
		id = strings.TrimPrefix(id, "models/") // Gemini
		if id == "" {
			continue
		}
		lm := liveModel{ID: id}
		_ = json.Unmarshal(d["description"], &lm.Description)
		for _, k := range []string{"context_length", "context_window", "max_context_length", "max_model_len"} {
			if n := jsonInt(d[k]); n > 0 {
				lm.Context = n
				break
			}
		}
		// OpenRouter: pricing, supported_parameters, architecture.
		if pr, ok := d["pricing"]; ok {
			var price struct{ Prompt, Completion string }
			if json.Unmarshal(pr, &price) == nil {
				lm.Free = isZero(price.Prompt) && isZero(price.Completion)
				lm.PriceIn, _ = strconv.ParseFloat(price.Prompt, 64)
				lm.PriceOut, _ = strconv.ParseFloat(price.Completion, 64)
			}
		}
		if sp, ok := d["supported_parameters"]; ok {
			var params []string
			if json.Unmarshal(sp, &params) == nil {
				t := contains(params, "tools")
				lm.Tools = &t
				th := contains(params, "reasoning") || contains(params, "include_reasoning")
				lm.Reasoning = &th
			}
		}
		if ar, ok := d["architecture"]; ok {
			var arch struct {
				InputModalities []string `json:"input_modalities"`
			}
			if json.Unmarshal(ar, &arch) == nil && arch.InputModalities != nil {
				v := contains(arch.InputModalities, "image")
				lm.Vision = &v
			}
		}
		// Mistral: capabilities.
		if cp, ok := d["capabilities"]; ok {
			var caps struct {
				FunctionCalling *bool `json:"function_calling"`
				Vision          *bool `json:"vision"`
			}
			if json.Unmarshal(cp, &caps) == nil {
				if caps.FunctionCalling != nil {
					lm.Tools = caps.FunctionCalling
				}
				if caps.Vision != nil {
					lm.Vision = caps.Vision
				}
			}
		}
		out = append(out, lm)
	}
	return out, nil
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.status, e.body) }

func jsonInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return int(n)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		i, _ := strconv.Atoi(s)
		return i
	}
	return 0
}

func isZero(s string) bool {
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == 0
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}
