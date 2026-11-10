package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Profile is a named set of filters and an order. An agent picks one by
// sending its name as the model: "fast", "vision", "cheap"...
type Profile struct {
	Require          []string `json:"require,omitempty"` // tools, vision, reasoning
	Exclude          []string `json:"exclude,omitempty"`
	FreeOnly         bool     `json:"free_only,omitempty"`
	MaxCost          *float64 `json:"max_cost,omitempty"` // USD per request; unset: ROUTER_MAX_COST
	MaxTTFTMs        int      `json:"max_ttft_ms,omitempty"`
	MinTPS           float64  `json:"min_tps,omitempty"`
	MinContext       int      `json:"min_context,omitempty"`
	MaxTier          int      `json:"max_tier,omitempty"` // 1 strongest
	Providers        []string `json:"providers,omitempty"`
	ExcludeProviders []string `json:"exclude_providers,omitempty"`
	Models           []string `json:"models,omitempty"` // globs on provider/id, e.g. "*coder*"
	NoTraining       bool     `json:"no_training,omitempty"`
	Domains          []string `json:"domains,omitempty"`     // good for all of these
	Sort             string   `json:"sort,omitempty"`        // strongest | fastest | cheapest | available
	Pinned           []string `json:"pinned,omitempty"`      // provider/id keys tried first, in this order
	OnlyPinned       bool     `json:"only_pinned,omitempty"` // use nothing but the pinned models
	Fallback         []string `json:"fallback,omitempty"`    // provider/id keys tried last, even outside the filters
	// Families, when set, limits the profile to these models, each from the
	// listed providers or, with no providers listed, from whoever serves it:
	// {"nemotron-3-super": [], "gpt-oss-120b": ["ovh"]}. The simple dashboard
	// builds profiles this way; a provider added later joins a model left open.
	Families map[string][]string `json:"families,omitempty"`
	// HedgeAfterMs, when set: a model with no first token this long after the
	// request went out gets company, the same request to the next model, and
	// whichever streams first answers. Faster, at up to twice the quota.
	HedgeAfterMs int `json:"hedge_after_ms,omitempty"`
}

var capabilityNames = []string{"tools", "vision", "reasoning"}

var sortNames = []string{"strongest", "fastest", "cheapest", "available"}

var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,40}$`)

func defaultProfiles() map[string]*Profile {
	return map[string]*Profile{
		"auto":     {Sort: "strongest"},
		"fast":     {Sort: "fastest"},
		"tools":    {Require: []string{"tools"}},
		"vision":   {Require: []string{"vision"}},
		"no-think": {Exclude: []string{"reasoning"}, Sort: "fastest"},
		"private":  {NoTraining: true},
	}
}

// profileAliases keeps older names working.
var profileAliases = map[string]string{"auto-fast": "fast"}

func (p *Profile) validate() error {
	for _, c := range append(slices.Clone(p.Require), p.Exclude...) {
		if !slices.Contains(capabilityNames, c) {
			return fmt.Errorf("unknown capability %q (known: %s)", c, strings.Join(capabilityNames, ", "))
		}
	}
	if err := validDomains(p.Domains); err != nil {
		return err
	}
	if p.Sort != "" && !slices.Contains(sortNames, p.Sort) {
		return fmt.Errorf("unknown sort %q (known: %s)", p.Sort, strings.Join(sortNames, ", "))
	}
	if p.MaxCost != nil && *p.MaxCost < 0 {
		return errors.New("max_cost must not be negative")
	}
	for _, g := range p.Models {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("bad model pattern %q: %w", g, err)
		}
	}
	for f := range p.Families {
		if strings.TrimSpace(f) == "" || f != strings.ToLower(f) {
			return fmt.Errorf("bad model family %q: lower case, as the dashboard lists them", f)
		}
	}
	return nil
}

// costCap is the most a paid model may cost per request under p.
func (r *Router) costCap(p *Profile) float64 {
	switch {
	case p == nil:
		return r.cfg.MaxCost
	case p.FreeOnly:
		return 0
	case p.MaxCost != nil:
		return *p.MaxCost
	}
	return r.cfg.MaxCost
}

// maxCostAny is the highest cap any profile allows; paid models are
// discovered only when it is above zero.
func (r *Router) maxCostAny() float64 {
	r.pmu.RLock()
	defer r.pmu.RUnlock()
	c := r.cfg.MaxCost
	for _, p := range r.profiles {
		if !p.FreeOnly && p.MaxCost != nil {
			c = max(c, *p.MaxCost)
		}
	}
	return c
}

func (m *Model) capability(name string) capability {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch name {
	case "tools":
		return m.tools
	case "vision":
		return m.vision
	case "reasoning":
		return m.reasoning
	}
	return capUnknown
}

// match says whether m passes p's filters, and if not, why. Price and the
// daily budget are checked by affordable; this is about the model itself.
// A requirement on an unknown capability fails: "vision" means known vision.
// An exclusion of an unknown capability passes.
func (p *Profile) match(m *Model) (bool, string) {
	for _, c := range p.Require {
		if m.capability(c) != capYes {
			return false, "no " + c
		}
	}
	for _, c := range p.Exclude {
		if m.capability(c) == capYes {
			return false, "has " + c
		}
	}
	if p.OnlyPinned && !slices.Contains(p.Pinned, m.Key) {
		return false, "not pinned"
	}
	if len(p.Families) > 0 {
		provs, ok := p.Families[m.Family]
		if !ok {
			return false, "model not chosen"
		}
		if len(provs) > 0 && !slices.Contains(provs, m.Provider.Spec.ID) {
			return false, "not from the chosen providers"
		}
	}
	if len(p.Domains) > 0 {
		have := m.domains()
		for _, d := range p.Domains {
			if !slices.Contains(have, d) {
				return false, "not marked for " + d
			}
		}
	}
	prov := m.Provider.Spec.ID
	if len(p.Providers) > 0 && !slices.Contains(p.Providers, prov) {
		return false, "provider not chosen"
	}
	if slices.Contains(p.ExcludeProviders, prov) {
		return false, "provider excluded"
	}
	if p.NoTraining && m.trains {
		return false, "trains on prompts"
	}
	if p.FreeOnly && m.paid() {
		return false, "paid"
	}
	if len(p.Models) > 0 && !slices.ContainsFunc(p.Models, func(g string) bool { return nameMatches(g, m.Key) }) {
		return false, "name does not match"
	}
	if p.MaxTier > 0 && m.Tier > p.MaxTier {
		return false, fmt.Sprintf("tier %d", m.Tier)
	}
	m.mu.Lock()
	ctx, ttft, tps := m.context, m.ttftMs, m.tps
	m.mu.Unlock()
	if p.MinContext > 0 && ctx < p.MinContext {
		if ctx == 0 {
			return false, "context unknown"
		}
		return false, fmt.Sprintf("context %d", ctx)
	}
	if p.MaxTTFTMs > 0 && (ttft == 0 || ttft > float64(p.MaxTTFTMs)) {
		if ttft == 0 {
			return false, "speed not measured"
		}
		return false, fmt.Sprintf("first token %.0f ms", ttft)
	}
	if p.MinTPS > 0 && (tps == 0 || tps < p.MinTPS) {
		if tps == 0 {
			return false, "speed not measured"
		}
		return false, fmt.Sprintf("%.0f tok/s", tps)
	}
	return true, ""
}

// order sorts models for p. Free models always come before paid ones.
func (p *Profile) order(list []*Model) {
	strongest := func(a, b *Model) int {
		if a.Tier != b.Tier {
			return a.Tier - b.Tier
		}
		return cmpFloat(a.sortTTFT(), b.sortTTFT())
	}
	var by func(a, b *Model) int
	switch p.Sort {
	case "fastest":
		by = func(a, b *Model) int {
			if c := cmpFloat(a.sortTTFT(), b.sortTTFT()); c != 0 {
				return c
			}
			return a.Tier - b.Tier
		}
	case "cheapest":
		by = func(a, b *Model) int {
			if c := cmpFloat(a.priceIn+a.priceOut, b.priceIn+b.priceOut); c != 0 {
				return c
			}
			return strongest(a, b)
		}
	case "available":
		by = func(a, b *Model) int {
			if c := cmpFloat(b.availability(), a.availability()); c != 0 {
				return c
			}
			return strongest(a, b)
		}
	default:
		by = strongest
	}
	slices.SortStableFunc(list, by)
	// Usually unreliable models after the others; then, above that, models
	// that just answered first, then untried ones, then those whose last
	// attempt failed; free before paid above all.
	slices.SortStableFunc(list, func(a, b *Model) int { return boolInt(a.rep.flaky()) - boolInt(b.rep.flaky()) })
	slices.SortStableFunc(list, func(a, b *Model) int { return int(a.health()) - int(b.health()) })
	slices.SortStableFunc(list, func(a, b *Model) int { return boolInt(a.paid()) - boolInt(b.paid()) })
	// Pinned models lead, in the order they were pinned; whatever is down
	// was already left out, so a pinned model that fails does not block.
	if len(p.Pinned) > 0 {
		pos := func(m *Model) int {
			if i := slices.Index(p.Pinned, m.Key); i >= 0 {
				return i
			}
			return len(p.Pinned)
		}
		slices.SortStableFunc(list, func(a, b *Model) int { return pos(a) - pos(b) })
	}
	// Fallback models go last, in the order they were set.
	if len(p.Fallback) > 0 {
		pos := func(m *Model) int { return slices.Index(p.Fallback, m.Key) + 1 } // 0: not a fallback
		slices.SortStableFunc(list, func(a, b *Model) int { return pos(a) - pos(b) })
	}
}

// availability is the share of recent attempts that worked; unknown is 0.5.
func (m *Model) availability() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := min(m.histN, historySize)
	if n == 0 {
		return 0.5
	}
	good := 0
	for i := range n {
		if m.history[i] {
			good++
		}
	}
	return float64(good) / float64(n)
}

// defaultOrder is what a name that is not a profile is served by: "auto"
// while it exists, else the plain order, strongest first. Deleting "auto"
// must not leave such requests without an order.
func (r *Router) defaultOrder() *Profile {
	if p := r.profile("auto"); p != nil {
		return p
	}
	return &Profile{}
}

// profile returns the profile a model name selects, or nil for a concrete model.
func (r *Router) profile(name string) *Profile {
	if a, ok := profileAliases[name]; ok {
		name = a
	}
	if name == "" {
		name = "auto"
	}
	r.pmu.RLock()
	defer r.pmu.RUnlock()
	return r.profiles[name]
}

func (r *Router) profilesPath() string {
	if p := os.Getenv("ROUTER_PROFILES"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "profiles.json")
	}
	return "profiles.json"
}

// profilesFile is the file's form since built-in profiles can be deleted:
// the file holds every profile there is. The older form, a bare map, was
// laid over the built-ins, so a deleted built-in would have come back.
type profilesFile struct {
	Version  int                 `json:"version"`
	Profiles map[string]*Profile `json:"profiles"`
}

// loadProfiles reads the file. Without one the built-in profiles are the
// start; a file in the older form is laid over them once and rewritten.
func (r *Router) loadProfiles() error {
	ps := defaultProfiles()
	rewrite := false
	b, err := os.ReadFile(r.profilesPath())
	switch {
	case err == nil:
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(b, &probe); err != nil {
			return fmt.Errorf("%s: %w", r.profilesPath(), err)
		}
		var file map[string]*Profile
		if _, ok := probe["version"]; ok {
			var f profilesFile
			if err := json.Unmarshal(b, &f); err != nil {
				return fmt.Errorf("%s: %w", r.profilesPath(), err)
			}
			ps, file = map[string]*Profile{}, f.Profiles
		} else if err := json.Unmarshal(b, &file); err != nil {
			return fmt.Errorf("%s: %w", r.profilesPath(), err)
		} else {
			rewrite = true
		}
		for name, p := range file {
			if err := p.validate(); err != nil {
				return fmt.Errorf("%s: profile %q: %w", r.profilesPath(), name, err)
			}
			ps[name] = p
		}
	case !os.IsNotExist(err):
		return err
	}
	r.pmu.Lock()
	r.profiles = ps
	r.pmu.Unlock()
	if rewrite {
		if err := r.saveProfiles(); err != nil {
			log.Printf("profiles: kept in the older form, could not rewrite: %v", err)
		}
	}
	return nil
}

// saveProfiles writes every profile to the file, so the file is the whole truth.
func (r *Router) saveProfiles() error {
	r.pmu.RLock()
	b, err := json.MarshalIndent(profilesFile{Version: 2, Profiles: r.profiles}, "", "  ")
	r.pmu.RUnlock()
	if err != nil {
		return err
	}
	tmp := r.profilesPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.profilesPath())
}

func (r *Router) profileNames() []string {
	r.pmu.RLock()
	defer r.pmu.RUnlock()
	names := make([]string, 0, len(r.profiles))
	for n := range r.profiles {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == "auto") != (names[j] == "auto") {
			return names[i] == "auto"
		}
		return names[i] < names[j]
	})
	return names
}

// typicalRequest is what the dashboard prices models by: 1K tokens in, 1K out.
var typicalRequest = &reqInfo{model: "auto", promptTokens: 1000, outTokens: 1000}

// matchEntry is one model as a profile sees it, for the dashboard preview.
type matchEntry struct {
	Key    string  `json:"key"`
	Match  bool    `json:"match"`
	Reason string  `json:"reason,omitempty"`
	Cost   float64 `json:"cost"` // USD for a typical request
}

// preview lists every model, matching ones first in the order p would try them.
func (r *Router) preview(p *Profile) []matchEntry {
	now := time.Now()
	r.mu.RLock()
	all := slices.Clone(r.models)
	r.mu.RUnlock()
	p.order(all)
	cap := r.costCap(p)
	// Matching models in the order they would be tried, then matching ones
	// that are down right now, then the rest.
	out := make([]matchEntry, 0, len(all))
	var down, miss []matchEntry
	for _, m := range all {
		e := matchEntry{Key: m.Key, Cost: m.estimateCost(typicalRequest)}
		ok, why := p.match(m)
		if !ok && slices.Contains(p.Fallback, m.Key) {
			ok, why = true, ""
		}
		switch {
		case !ok:
		case m.paid() && cap <= 0:
			ok, why = false, "paid (cost cap is 0)"
		case m.paid() && e.Cost > cap:
			ok, why = false, fmt.Sprintf("costs $%.5f", e.Cost)
		}
		e.Match, e.Reason = ok, why
		switch {
		case !ok:
			miss = append(miss, e)
		case !m.usable(now):
			e.Match, e.Reason = false, "down now: "+m.status(now).State
			down = append(down, e)
		default:
			out = append(out, e)
		}
	}
	return append(append(out, down...), miss...)
}

// nameMatches reads a model pattern the way people write it: "*" is any text,
// slashes included, so "*gpt*" finds ovh/gpt-oss-20b and "kilo/*" every kilo
// model. path.Match alone stops "*" at a slash.
func nameMatches(pattern, key string) bool {
	slash := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "/", "\x1f") }
	ok, _ := path.Match(slash(pattern), slash(key))
	return ok
}
