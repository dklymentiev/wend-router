package main

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// The provider list is rebuilt whenever keys or settings change in the
// dashboard: seed providers plus custom ones, each with its keys from env
// and from the key store. A provider whose keys changed keeps its object
// (and its models' history); one whose address changed gets a new object.

// allSpecs is every provider the router knows, with the dashboard's on/off
// choices applied: Disabled holds why a provider is off, empty when on.
func (r *Router) allSpecs() []ProviderSpec {
	var d storeData
	if r.store != nil {
		d = r.store.view()
	}
	specs := slices.Clone(r.seed.Providers)
	for _, c := range d.Custom {
		c.Custom = true
		specs = append(specs, c)
	}
	for i := range specs {
		id := specs[i].ID
		if d.Enabled[id] {
			specs[i].Disabled = ""
		}
		if why, ok := d.Disabled[id]; ok {
			specs[i].Disabled = why
		}
	}
	return specs
}

// getenv reads env first, then the values saved in the dashboard (such as
// Cloudflare's account id).
func (r *Router) lookup(name string) string {
	if v := r.getenv(name); v != "" {
		return v
	}
	if r.store != nil {
		return r.store.view().Vars[name]
	}
	return ""
}

func (r *Router) storeKeys(id string) []providerKey {
	if r.store == nil {
		return nil
	}
	var out []providerKey
	for _, k := range r.store.view().Keys[id] {
		out = append(out, providerKey{ID: k.ID, Value: k.Key, Label: k.Label, Source: "dashboard"})
	}
	return out
}

func specJSON(s ProviderSpec) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// reloadProviders rebuilds the provider list and drops the models of
// providers that are gone. Call Discover afterwards to find new models.
func (r *Router) reloadProviders() {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := map[string]*Provider{}
	for _, p := range r.providers {
		old[p.Spec.ID] = p
	}
	var next []*Provider
	for _, spec := range r.allSpecs() {
		if spec.Disabled != "" || (!r.cfg.AllowTraining && spec.TrainsOnFree) {
			continue
		}
		p := newProvider(spec, r.lookup, r.storeKeys(spec.ID)...)
		if p == nil {
			continue
		}
		if o := old[spec.ID]; o != nil && o.BaseURL == p.BaseURL && specJSON(o.Spec) == specJSON(p.Spec) {
			o.setKeys(p.keys)
			p = o
		} else if o != nil {
			p.rep.restore(o.rep.snapshot())
		} else if rep, ok := r.pendingProviders[spec.ID]; ok {
			p.rep.restore(rep)
		}
		next = append(next, p)
	}
	r.providers = next
	keep := map[*Provider]bool{}
	for _, p := range next {
		keep[p] = true
	}
	r.models = slices.DeleteFunc(r.models, func(m *Model) bool { return !keep[m.Provider] })
}

func (r *Router) providerList() []*Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.providers)
}

func (r *Router) providerByID(id string) *Provider {
	for _, p := range r.providerList() {
		if p.Spec.ID == id {
			return p
		}
	}
	return nil
}

// providerState is one line of truth about a provider, for the start-up
// table and the dashboard.
func (r *Router) providerState(spec ProviderSpec, p *Provider, models int) string {
	switch {
	case spec.Disabled != "":
		return "disabled"
	case !r.cfg.AllowTraining && spec.TrainsOnFree:
		return "excluded: trains on prompts"
	case p == nil && len(urlVars(spec.BaseURL)) > 0 && r.lookup(urlVars(spec.BaseURL)[0]) == "":
		return "needs " + strings.Join(urlVars(spec.BaseURL), ", ")
	case p == nil:
		return "no key"
	case !p.usable(time.Now()):
		p.mu.Lock()
		defer p.mu.Unlock()
		return "off: " + p.offWhy
	}
	p.mu.Lock()
	listErr := p.listErr
	p.mu.Unlock()
	switch {
	case models == 0 && listErr != "":
		return listErr
	case models == 0:
		return "no models"
	}
	return "ok"
}

func (r *Router) modelCounts() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := map[string]int{}
	for _, m := range r.models {
		count[m.Provider.Spec.ID]++
	}
	return count
}
