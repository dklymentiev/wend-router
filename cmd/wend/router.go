package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

type Config struct {
	TTFTTimeout    time.Duration // no first token by then: next model
	TTFTMin        time.Duration // the shortest first-token deadline for a model with a known speed; 0: always TTFTTimeout
	StallTimeout   time.Duration // nothing from the model this long after its first token: give up on it; 0: no limit
	RequestTimeout time.Duration // whole non-streaming request
	MaxAttempts    int
	AllowTraining  bool          // use providers whose free tier may train on prompts
	Token          string        // when set, clients must send it as a bearer token
	MaxCost        float64       // USD a paid model may cost per request; 0 keeps to free models
	DailyBudget    float64       // USD a day across paid models
	ProbeEvery     time.Duration // re-check idle models this often
	MaxWait        time.Duration // how long a request may wait for a paused model
}

// Router holds the providers and models and picks one for each request.
type Router struct {
	cfg       Config
	seed      *Seed
	getenv    func(string) string
	store     *keyStore   // nil when keys come from env only
	providers []*Provider // guarded by mu
	client    *http.Client
	pipeline  []Transform
	filter    *termFilter // the Filter tab's term list; also first in pipeline
	live      *liveHub
	pipe      *pipeHub // the raw traffic, live, for the Pipe tab
	book      *sessionBook
	usage     *usageLedger
	traffic   *trafficLog
	pmu       sync.RWMutex
	profiles  map[string]*Profile
	spent     ledger

	mu               sync.RWMutex
	models           []*Model
	sessions         map[string]string     // session id -> model key it last worked on
	pending          map[string]modelState // saved state for models not discovered yet
	pendingProviders map[string]repData    // saved reputation for providers not on yet
	testMu           sync.Mutex
	runMu            sync.Mutex
	run              *testRun           // the dashboard's "Test all", if any
	testing          map[*Provider]bool // providers with a test run in progress
}

func NewRouter(seed *Seed, cfg Config, getenv func(string) string) *Router {
	r := &Router{
		cfg:      cfg,
		seed:     seed,
		getenv:   getenv,
		sessions: map[string]string{},
		live:     newLiveHub(),
		pipe:     newPipeHub(),
		book:     newSessionBook(),
		usage:    newUsageLedger(),
		traffic:  newTrafficLog(),
		testing:  map[*Provider]bool{},
		profiles: defaultProfiles(),
		client: &http.Client{Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		}},
	}
	r.filter = newTermFilter(filterPath())
	r.pipeline = []Transform{r.filter}
	r.reloadProviders()
	return r
}

// Discover asks every provider which models it serves now and merges the
// answer into the model list, keeping what was learned about known models.
func (r *Router) Discover(ctx context.Context) {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		found []*Model
	)
	for _, p := range r.providerList() {
		wg.Go(func() {
			ms, err := r.discoverProvider(ctx, p)
			p.mu.Lock()
			p.listErr = ""
			if err != nil {
				p.listErr = err.Error()
			}
			p.mu.Unlock()
			if err != nil {
				log.Printf("%s: %v", p.Spec.ID, err)
			}
			mu.Lock()
			found = append(found, ms...)
			mu.Unlock()
		})
	}
	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	current := map[*Provider]bool{}
	for _, p := range r.providers {
		current[p] = true
	}
	// A provider removed while discovery ran keeps none of its models.
	found = slices.DeleteFunc(found, func(m *Model) bool { return !current[m.Provider] })
	old := map[string]*Model{}
	for _, m := range r.models {
		old[m.Key] = m
	}
	merged := make([]*Model, 0, len(found))
	for _, m := range found {
		if s, ok := r.pending[m.Key]; ok {
			m.loadState(s)
			delete(r.pending, m.Key)
		}
		if prev, ok := old[m.Key]; ok {
			prev.mu.Lock()
			prev.tags = m.tags
			prev.seedTier = m.Tier
			if prev.test == nil {
				prev.Tier = m.Tier
			}
			// Listed again, so not gone; a refusal (404, paid-only) keeps
			// its own expiry: being listed does not mean being served.
			prev.gone = false
			prev.mu.Unlock()
			m = prev
		}
		merged = append(merged, m)
	}
	slices.SortFunc(merged, func(a, b *Model) int {
		if a.Tier != b.Tier {
			return a.Tier - b.Tier
		}
		return strings.Compare(a.Key, b.Key)
	})
	r.models = merged
}

func (r *Router) discoverProvider(ctx context.Context, p *Provider) ([]*Model, error) {
	spec := p.Spec
	if spec.Discover == "seed-only" { // the provider has no usable /models
		return r.seedModels(p), nil
	}
	live, err := p.listModels(ctx, r.client)
	// A refused key moves to the next one; try each key once.
	for tries := len(p.keyList()); err != nil && tries > 0; tries-- {
		var he *httpError
		if !errors.As(err, &he) || (he.status != 401 && he.status != 403) {
			break
		}
		p.refuseKey("key refused")
		if !p.usable(time.Now()) {
			return nil, fmt.Errorf("key refused: %w", err)
		}
		live, err = p.listModels(ctx, r.client)
	}
	if err != nil {
		var he *httpError
		if errors.As(err, &he) && (he.status == 401 || he.status == 403) {
			p.refuseKey("key refused")
			return nil, fmt.Errorf("key refused: %w", err)
		}
		if spec.NoKey {
			return nil, fmt.Errorf("not reachable at %s", p.BaseURL)
		}
		// The list failed for another reason: fall back to the seed's own
		// models, which were checked when the seed was written.
		return r.seedModels(p), fmt.Errorf("models list failed, using seed models: %w", err)
	}
	seedByID := map[string]ModelSpec{}
	for _, ms := range spec.Models {
		seedByID[ms.ID] = ms
	}
	var out []*Model
	for _, lm := range live {
		ms, inSeed := seedByID[lm.ID]
		switch spec.Discover {
		case "seed":
			if !inSeed {
				continue
			}
		case "free":
			// Free models, and paid ones too when a cost cap allows paid use.
			// Routers ("openrouter/auto") and varying prices are left out.
			if !inSeed && ((lm.Tools != nil && !*lm.Tools) || strings.HasPrefix(lm.ID, "openrouter/") || excluded(lm.ID, spec.Exclude) ||
				lm.PriceIn < 0 || lm.PriceOut < 0 ||
				(!lm.Free && 1000*(lm.PriceIn+lm.PriceOut) > r.maxCostAny())) {
				continue
			}
		default: // "all"
			if !inSeed && excluded(lm.ID, spec.Exclude) {
				continue
			}
		}
		m := r.newModel(p, lm.ID, ms)
		if m.context == 0 {
			m.context = lm.Context
		}
		if m.tools == capUnknown {
			m.tools = capFrom(lm.Tools)
		}
		if m.vision == capUnknown {
			m.vision = capFrom(lm.Vision)
		}
		if m.reasoning == capUnknown {
			m.reasoning = capFrom(lm.Reasoning)
		}
		m.tags = append(m.tags, guessDomains(lm.ID, lm.Description)...)
		// Prices count only where the provider's list says what is free
		// (OpenRouter). Elsewhere the list shows paid-tier prices for models
		// the free tier serves at no cost (Groq), and the seed decides.
		if spec.Discover == "free" && !lm.Free {
			m.priceIn, m.priceOut = max(lm.PriceIn, 0), max(lm.PriceOut, 0)
		}
		out = append(out, m)
	}
	for id := range seedByID {
		if !slices.ContainsFunc(live, func(lm liveModel) bool { return lm.ID == id }) {
			log.Printf("%s: seed model %q is not in the provider's list, skipped", spec.ID, id)
		}
	}
	return out, nil
}

func (r *Router) seedModels(p *Provider) []*Model {
	out := make([]*Model, 0, len(p.Spec.Models))
	for _, ms := range p.Spec.Models {
		out = append(out, r.newModel(p, ms.ID, ms))
	}
	return out
}

func (r *Router) newModel(p *Provider, id string, ms ModelSpec) *Model {
	tier := ms.Tier
	if tier == 0 {
		tier = p.Spec.DefaultTier
	}
	if tier == 0 {
		tier = 3
	}
	trains := p.Spec.TrainsOnFree
	if ms.Trains != nil {
		trains = *ms.Trains
	}
	return &Model{
		seedTier: tier,
		tags:     append(guessDomains(id, ""), ms.Domains...),
		Provider: p, ID: id, Key: p.Spec.ID + "/" + id, Family: familyOf(id, ms.Family), Tier: tier, trains: trains,
		context: ms.Context, tools: capFrom(ms.Tools), vision: capFrom(ms.Vision),
	}
}

// familyOf names the model itself, so the same model served by several
// providers can be chosen once: the seed's family when it gives one (names
// that differ between providers), else the id's last part in lower case,
// without a variant tag ("nvidia/Nemotron-3-Super:free" -> "nemotron-3-super")
// but with a size tag, which names a different model ("gpt-oss:120b" ->
// "gpt-oss-120b", as other providers call it).
func familyOf(id, seed string) string {
	if seed != "" {
		return strings.ToLower(seed)
	}
	f := strings.ToLower(id[strings.LastIndex(id, "/")+1:])
	if i := strings.Index(f, ":"); i > 0 {
		if tag := f[i+1:]; sizeTag.MatchString(tag) {
			return f[:i] + "-" + tag
		}
		f = f[:i]
	}
	return f
}

var sizeTag = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[bm]$`)

func excluded(id string, words []string) bool {
	low := strings.ToLower(id)
	for _, w := range words {
		if strings.Contains(low, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

// RefreshLoop re-runs discovery so new and retired models are picked up.
func (r *Router) RefreshLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Discover(ctx)
		}
	}
}

// scope is what a request may be served by, and the order to try it in. A
// profile: its own models and the fallbacks it names. "provider/model": that
// one model at that provider, so a measurement measures what it asked for. A
// bare model name: that model at every provider that serves it (its family).
// Nothing else: when none of them can answer, the caller gets an error within
// seconds rather than an answer, minutes later, from a model it never asked for.
func (r *Router) scope(ri *reqInfo, all []*Model) (*Profile, func(*Model) bool) {
	if p := r.profile(ri.model); p != nil {
		return p, func(m *Model) bool {
			ok, _ := p.match(m)
			return ok || slices.Contains(p.Fallback, m.Key)
		}
	}
	for _, m := range all {
		if m.Key == ri.model {
			return r.defaultOrder(), func(x *Model) bool { return x == m }
		}
	}
	fam := familyOf(ri.model, "")
	for _, m := range all {
		if m.ID == ri.model {
			fam = m.Family
			break
		}
	}
	return r.defaultOrder(), func(m *Model) bool { return m.ID == ri.model || m.Family == fam }
}

// unknown reports a name that is neither a profile nor a model any provider serves.
func (r *Router) unknown(name string) bool {
	if r.profile(name) != nil {
		return false
	}
	fam := familyOf(name, "")
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.models {
		if m.Key == name || m.ID == name || m.Family == fam {
			return false
		}
	}
	return true
}

// candidates returns the models to try for a request, best first, from its
// scope. A concrete model name puts that model first.
func (r *Router) candidates(ri *reqInfo) []*Model {
	now := time.Now()
	r.mu.RLock()
	all := slices.Clone(r.models)
	sticky := r.sessions[stickyKey(ri)]
	r.mu.RUnlock()

	p, in := r.scope(ri, all)
	cap := r.costCap(p)
	var list []*Model
	for _, m := range all {
		if in(m) && m.usable(now) && m.hasRoom() && m.fits(ri) && m.tokensFit(ri, now) && r.affordable(m, ri, cap) {
			list = append(list, m)
		}
	}
	p.order(list)
	// Known vision models first for a request with an image.
	if ri.hasImage {
		slices.SortStableFunc(list, func(a, b *Model) int { return visionRank(a) - visionRank(b) })
	}
	front := sticky
	if r.profile(ri.model) == nil {
		front = ri.model
	}
	if front != "" {
		if i := slices.IndexFunc(list, func(m *Model) bool { return m.Key == front || m.ID == front }); i > 0 {
			m := list[i]
			list = slices.Delete(list, i, i+1)
			list = slices.Insert(list, 0, m)
		}
	}
	return list
}

func visionRank(m *Model) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vision == capYes {
		return 0
	}
	return 1
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (r *Router) remember(session, key string) {
	if session == "" {
		return
	}
	r.mu.Lock()
	r.sessions[session] = key
	r.mu.Unlock()
}

func (r *Router) snapshot() []modelStatus {
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]modelStatus, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, m.status(now))
	}
	return out
}

// PrintTable writes the start-up summary: providers, then models.
func (r *Router) PrintTable(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tSTATE\tMODELS\tTRAINS ON FREE")
	count := r.modelCounts()
	for _, spec := range r.allSpecs() {
		p := r.providerByID(spec.ID)
		state := r.providerState(spec, p, count[spec.ID])
		if state == "disabled" {
			state += ": " + spec.Disabled
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%v\n", spec.ID, truncate(state, 60), count[spec.ID], spec.TrainsOnFree)
	}
	tw.Flush()
	fmt.Fprintln(w)
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIER\tMODEL\tCONTEXT\tTOOLS\tVISION")
	for _, s := range r.snapshot() {
		ctx := "?"
		if s.Context > 0 {
			ctx = fmt.Sprint(s.Context)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", s.Tier, s.Key, ctx, s.Tools, s.Vision)
	}
	tw.Flush()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// stickyKey is what a model is kept for: one profile within one session. A
// client's classifier and its agent share a session but not a model (an
// agent may run a classifier on a fast model without thinking and itself on tools).
func stickyKey(ri *reqInfo) string {
	if ri.session == "" {
		return ""
	}
	return ri.session + "|" + ri.model
}

// nextFree is when the first model the request could use comes back from
// a short pause (a quota window, an open breaker); zero when none will.
func (r *Router) nextFree(ri *reqInfo) time.Time {
	now := time.Now()
	var soonest time.Time
	r.mu.RLock()
	all := slices.Clone(r.models)
	r.mu.RUnlock()
	p, in := r.scope(ri, all)
	for _, m := range all {
		if !in(m) || !m.Provider.usable(now) || !m.fits(ri) || !r.affordable(m, ri, r.costCap(p)) {
			continue
		}
		m.mu.Lock()
		gone := m.gone || now.Before(m.goneUntil)
		until := latest(m.pausedUntil, m.openUntil)
		m.mu.Unlock()
		if gone || !until.After(now) {
			continue
		}
		if soonest.IsZero() || until.Before(soonest) {
			soonest = until
		}
	}
	return soonest
}

// waitForModel holds a request while every model it may use is on a short
// pause, up to MaxWait, instead of failing it at once. A channel pinned to
// one model (a benchmark, a profile of one model) then rides out a rate limit.
func (r *Router) waitForModel(ctx context.Context, ri *reqInfo) []*Model {
	deadline := time.Now().Add(r.cfg.MaxWait)
	for {
		next := r.nextFree(ri)
		if next.IsZero() || next.After(deadline) {
			return nil
		}
		log.Printf("waiting %s for a model of %q", time.Until(next).Round(time.Second), nonEmptyOr(ri.model, "auto"))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Until(next) + 200*time.Millisecond):
		}
		if c := r.candidates(ri); len(c) > 0 {
			return c
		}
	}
}
