package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

func (r *Router) handleProfiles(w http.ResponseWriter, _ *http.Request) {
	r.pmu.RLock()
	profiles := make(map[string]*Profile, len(r.profiles))
	for n, p := range r.profiles {
		profiles[n] = p
	}
	r.pmu.RUnlock()
	var providers []string
	for _, p := range r.providerList() {
		providers = append(providers, p.Spec.ID)
	}
	builtin := make([]string, 0)
	for n := range defaultProfiles() {
		builtin = append(builtin, n)
	}
	slices.Sort(builtin)
	writeJSON(w, map[string]any{
		"names": r.profileNames(), "profiles": profiles, "builtin": builtin,
		"capabilities": capabilityNames, "sorts": sortNames, "domains": domainNames, "providers": providers,
		"default_max_cost": r.cfg.MaxCost, "budget": r.cfg.DailyBudget, "families": r.families(),
	})
}

// familyModel is one model of a family, at one provider, as the profile builder shows it.
type familyModel struct {
	Key      string `json:"key"`
	Provider string `json:"provider"`
	Usable   bool   `json:"usable"`
	Trains   bool   `json:"trains"`
	Tools    bool   `json:"tools"`
	Paid     bool   `json:"paid"`
}

type familyView struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Tier   int           `json:"tier"` // the strongest of its models
	Models []familyModel `json:"models"`
}

// families groups the known models by the model itself, strongest first:
// what the simple dashboard lets a profile pick, each with its providers.
func (r *Router) families() []familyView {
	now := time.Now()
	r.mu.RLock()
	all := slices.Clone(r.models)
	r.mu.RUnlock()
	byID := map[string]*familyView{}
	var order []string
	for _, m := range all {
		f := byID[m.Family]
		if f == nil {
			name := m.ID[strings.LastIndex(m.ID, "/")+1:]
			if i := strings.Index(name, ":"); i > 0 {
				name = name[:i]
			}
			f = &familyView{ID: m.Family, Name: name, Tier: m.Tier}
			byID[m.Family] = f
			order = append(order, m.Family)
		}
		f.Tier = min(f.Tier, m.Tier)
		f.Models = append(f.Models, familyModel{Key: m.Key, Provider: m.Provider.Spec.ID, Usable: m.usable(now),
			Trains: m.trains, Tools: m.capability("tools") == capYes, Paid: m.paid()})
	}
	out := make([]familyView, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	slices.SortStableFunc(out, func(a, b familyView) int {
		if a.Tier != b.Tier {
			return a.Tier - b.Tier
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func readProfile(req *http.Request) (*Profile, error) {
	b, err := io.ReadAll(io.LimitReader(req.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	var p Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, p.validate()
}

func (r *Router) handleProfilePut(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	if !profileName.MatchString(name) {
		writeError(w, 400, "profile names are lowercase letters, digits, dot, dash and underscore", "invalid_request_error")
		return
	}
	p, err := readProfile(req)
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	before := r.maxCostAny()
	r.pmu.Lock()
	r.profiles[name] = p
	r.pmu.Unlock()
	if err := r.saveProfiles(); err != nil {
		writeError(w, 500, "saved in memory, not on disk: "+err.Error(), "router_error")
		return
	}
	// A higher cost cap lets more paid models in: look again.
	if r.maxCostAny() > before {
		go r.Discover(context.Background())
	}
	writeJSON(w, map[string]any{"saved": name, "profile": p})
}

func (r *Router) handleProfileDelete(w http.ResponseWriter, req *http.Request) {
	// Built-in profiles are starting examples and go like any other; a name
	// that is no longer a profile is served by the default order.
	name := req.PathValue("name")
	r.pmu.Lock()
	_, ok := r.profiles[name]
	delete(r.profiles, name)
	r.pmu.Unlock()
	if !ok {
		writeError(w, 404, "no such profile", "invalid_request_error")
		return
	}
	if err := r.saveProfiles(); err != nil {
		writeError(w, 500, err.Error(), "router_error")
		return
	}
	writeJSON(w, map[string]any{"deleted": name})
}

// handlePreview shows how a draft profile would pick models, without saving it.
func (r *Router) handlePreview(w http.ResponseWriter, req *http.Request) {
	p, err := readProfile(req)
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	writeJSON(w, map[string]any{"models": r.preview(p), "cap": r.costCap(p)})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
