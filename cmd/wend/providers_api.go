package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// Dashboard API for providers and keys. Keys go in, never out: the list
// shows a masked form only.

type keyView struct {
	ID      string `json:"id"`
	Masked  string `json:"masked"`
	Label   string `json:"label,omitempty"`
	Source  string `json:"source"` // env or dashboard
	InUse   bool   `json:"in_use"`
	Refused string `json:"refused,omitempty"`
}

type providerView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	KeyNote string `json:"key_note,omitempty"`
	// Listed: the simple dashboard shows it as the user's (it has a key or needs
	// none, and was neither removed nor shipped off); the rest wait in the list
	// of providers to add. On: not turned off.
	Listed    bool              `json:"listed"`
	On        bool              `json:"on"`
	BaseURL   string            `json:"base_url"`
	State     string            `json:"state"`
	Disabled  string            `json:"disabled,omitempty"`
	Note      string            `json:"note,omitempty"`
	SignupURL string            `json:"signup_url,omitempty"`
	KeyMode   string            `json:"key_mode"` // required or optional
	Trains    bool              `json:"trains"`
	Custom    bool              `json:"custom"`
	Vars      map[string]string `json:"vars,omitempty"` // {NAME} in the URL -> its value, empty when missing
	Models    int               `json:"models"`
	Keys      []keyView         `json:"keys"`
	Usually   string            `json:"usually"`
}

// keyMode: every provider takes a key; one that works without is "optional",
// and its key_note says what a key changes.
func keyMode(s ProviderSpec) string {
	if s.NoKey {
		return "optional"
	}
	return "required"
}

func displayName(s ProviderSpec) string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

func (r *Router) handleProviders(w http.ResponseWriter, _ *http.Request) {
	count := r.modelCounts()
	var out []providerView
	for _, spec := range r.allSpecs() {
		p := r.providerByID(spec.ID)
		v := providerView{
			ID: spec.ID, Name: displayName(spec), KeyNote: spec.KeyNote, On: spec.Disabled == "",
			BaseURL: spec.BaseURL, Disabled: spec.Disabled, Note: spec.Note, SignupURL: spec.SignupURL,
			KeyMode: keyMode(spec), Trains: spec.TrainsOnFree, Custom: spec.Custom, Models: count[spec.ID],
			State: r.providerState(spec, p, count[spec.ID]),
		}
		for _, name := range urlVars(spec.BaseURL) {
			if v.Vars == nil {
				v.Vars = map[string]string{}
			}
			v.Vars[name] = r.lookup(name)
		}
		keys := r.storeKeys(spec.ID)
		cur := ""
		v.Usually = "new"
		if p != nil {
			v.Usually = p.rep.label()
			keys = p.keyList()
			p.mu.Lock()
			if p.cur < len(p.keys) {
				cur = p.keys[p.cur].ID
			}
			p.mu.Unlock()
		} else {
			for _, name := range spec.KeyEnv {
				if val := strings.TrimSpace(r.getenv(name)); val != "" {
					keys = append([]providerKey{{ID: keyID(val), Value: val, Label: name, Source: "env"}}, keys...)
				}
			}
		}
		for _, k := range keys {
			v.Keys = append(v.Keys, keyView{ID: k.ID, Masked: maskKey(k.Value), Label: k.Label, Source: k.Source, InUse: k.ID == cur, Refused: k.BadWhy})
		}
		// Off by the user's switch keeps it listed; removed, or off in the seed
		// (a provider whose free tier ended), puts it back among those to add.
		offBySwitch := strings.HasPrefix(spec.Disabled, offInDashboard)
		v.Listed = (len(v.Keys) > 0 || spec.NoKey) && (spec.Disabled == "" || offBySwitch)
		out = append(out, v)
	}
	writeJSON(w, map[string]any{"providers": out, "store": r.store != nil})
}

// ui guards the dashboard's changing calls. A browser sends a custom header
// only from a page of the same origin (anything else needs a CORS preflight
// the router never answers), and the Host check stops DNS rebinding. Without
// both, any web page could point a provider at its own server and collect
// the key.
func (r *Router) ui(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		host, _, err := net.SplitHostPort(req.Host)
		if err != nil {
			host = req.Host
		}
		local := host == "localhost" || host == "127.0.0.1" || host == "::1"
		if !local && os.Getenv("ROUTER_REMOTE_UI") != "1" {
			writeError(w, 403, "dashboard changes are accepted on localhost only (ROUTER_REMOTE_UI=1 allows more)", "forbidden")
			return
		}
		if req.Header.Get("X-Router-UI") != "1" {
			writeError(w, 403, "missing X-Router-UI header", "forbidden")
			return
		}
		if r.cfg.Token != "" && req.Header.Get("X-Router-Token") != r.cfg.Token && req.Header.Get("Authorization") != "Bearer "+r.cfg.Token {
			writeError(w, 401, "router token required", "unauthorized")
			return
		}
		next(w, req)
	}
}

func readJSON(req *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(req.Body, 64<<10))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (r *Router) specByID(id string) (ProviderSpec, bool) {
	for _, s := range r.allSpecs() {
		if s.ID == id {
			return s, true
		}
	}
	return ProviderSpec{}, false
}

// checkKey asks the provider's model list with the key. A refusal is an
// error; any other failure is reported but does not block saving.
func (r *Router) checkKey(spec ProviderSpec, key string) (warning string, err error) {
	spec.Disabled = ""
	p := newProvider(spec, r.lookup, providerKey{ID: keyID(key), Value: key, Source: "dashboard"})
	if p == nil {
		return "", errors.New("the provider's address needs values that are not set: " + strings.Join(urlVars(spec.BaseURL), ", "))
	}
	if spec.Discover == "seed-only" {
		return "not verified: this provider has no model list; the prober will check it", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, lerr := p.listModels(ctx, r.client)
	var he *httpError
	if errors.As(lerr, &he) && (he.status == 401 || he.status == 403) {
		return "", errors.New("the provider refused this key: " + he.body)
	}
	if lerr != nil {
		return "saved, but not verified: " + lerr.Error(), nil
	}
	return "", nil
}

// applyChange rebuilds providers and, in the background, finds and checks
// the models a change brought in.
func (r *Router) applyChange(providerID string) {
	r.reloadProviders()
	go func() {
		ctx := context.Background()
		r.Discover(ctx)
		var fresh []*Model
		r.mu.RLock()
		for _, m := range r.models {
			if m.Provider.Spec.ID == providerID && m.health() == healthUntested && !m.paid() {
				fresh = append(fresh, m)
			}
		}
		r.mu.RUnlock()
		r.probeAll(ctx, fresh)
	}()
}

func (r *Router) needStore(w http.ResponseWriter) bool {
	if r.store == nil {
		writeError(w, 500, "the key store is not open; see the router log", "router_error")
		return false
	}
	return true
}

func (r *Router) handleKeyAdd(w http.ResponseWriter, req *http.Request) {
	if !r.needStore(w) {
		return
	}
	id := req.PathValue("id")
	spec, ok := r.specByID(id)
	if !ok {
		writeError(w, 404, "no such provider", "invalid_request_error")
		return
	}
	var body struct{ Key, Label string }
	if err := readJSON(req, &body); err != nil || strings.TrimSpace(body.Key) == "" {
		writeError(w, 400, "send {\"key\": \"...\", \"label\": \"optional\"}", "invalid_request_error")
		return
	}
	key := strings.TrimSpace(body.Key)
	warning, err := r.checkKey(spec, key)
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	err = r.store.update(func(d *storeData) error {
		if d.Keys == nil {
			d.Keys = map[string][]storedKey{}
		}
		for _, k := range d.Keys[id] {
			if k.Key == key {
				return errors.New("this key is already stored")
			}
		}
		d.Keys[id] = append(d.Keys[id], storedKey{ID: keyID(key), Key: key, Label: strings.TrimSpace(body.Label), Added: time.Now()})
		// A key is added to use the provider: one that was off or removed comes back.
		delete(d.Disabled, id)
		if d.Enabled == nil {
			d.Enabled = map[string]bool{}
		}
		d.Enabled[id] = true
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	r.applyChange(id)
	writeJSON(w, map[string]any{"added": maskKey(key), "warning": warning})
}

func (r *Router) handleKeyDelete(w http.ResponseWriter, req *http.Request) {
	if !r.needStore(w) {
		return
	}
	id, kid := req.PathValue("id"), req.PathValue("kid")
	found := false
	err := r.store.update(func(d *storeData) error {
		before := len(d.Keys[id])
		d.Keys[id] = slices.DeleteFunc(d.Keys[id], func(k storedKey) bool { return k.ID == kid })
		found = len(d.Keys[id]) < before
		if len(d.Keys[id]) == 0 {
			delete(d.Keys, id)
		}
		return nil
	})
	switch {
	case err != nil:
		writeError(w, 500, err.Error(), "router_error")
	case !found:
		writeError(w, 404, "no such key in the dashboard store (keys from wend.env are removed there)", "invalid_request_error")
	default:
		r.applyChange(id)
		writeJSON(w, map[string]any{"deleted": kid})
	}
}

func (r *Router) handleVars(w http.ResponseWriter, req *http.Request) {
	if !r.needStore(w) {
		return
	}
	id := req.PathValue("id")
	spec, ok := r.specByID(id)
	if !ok {
		writeError(w, 404, "no such provider", "invalid_request_error")
		return
	}
	var vars map[string]string
	if err := readJSON(req, &vars); err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	allowed := urlVars(spec.BaseURL)
	err := r.store.update(func(d *storeData) error {
		for k, v := range vars {
			if !slices.Contains(allowed, k) {
				return errors.New(k + " is not used by this provider")
			}
			if d.Vars == nil {
				d.Vars = map[string]string{}
			}
			d.Vars[k] = strings.TrimSpace(v)
		}
		return nil
	})
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	r.applyChange(id)
	writeJSON(w, map[string]any{"saved": len(vars)})
}

func (r *Router) handleEnabled(w http.ResponseWriter, req *http.Request) {
	if !r.needStore(w) {
		return
	}
	id := req.PathValue("id")
	if _, ok := r.specByID(id); !ok {
		writeError(w, 404, "no such provider", "invalid_request_error")
		return
	}
	var body struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err := readJSON(req, &body); err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	err := r.store.update(func(d *storeData) error {
		if d.Disabled == nil {
			d.Disabled = map[string]string{}
		}
		if d.Enabled == nil {
			d.Enabled = map[string]bool{}
		}
		if body.Enabled {
			delete(d.Disabled, id)
			d.Enabled[id] = true
		} else {
			delete(d.Enabled, id)
			d.Disabled[id] = offInDashboard + time.Now().Format("2006-01-02") + nonEmpty(": "+body.Reason, body.Reason)
		}
		return nil
	})
	if err != nil {
		writeError(w, 500, err.Error(), "router_error")
		return
	}
	r.applyChange(id)
	writeJSON(w, map[string]any{"id": id, "enabled": body.Enabled})
}

// offInDashboard starts the reason of a provider turned off with its switch.
const offInDashboard = "turned off in the dashboard "

func nonEmpty(s, check string) string {
	if strings.TrimSpace(check) == "" {
		return ""
	}
	return s
}

func (r *Router) handleCustomAdd(w http.ResponseWriter, req *http.Request) {
	if !r.needStore(w) {
		return
	}
	var body struct {
		ID           string `json:"id"`
		BaseURL      string `json:"base_url"`
		Key          string `json:"key"`
		Discover     string `json:"discover"`
		TrainsOnFree bool   `json:"trains_on_free"`
		SignupURL    string `json:"signup_url"`
	}
	if err := readJSON(req, &body); err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	body.ID = strings.ToLower(strings.TrimSpace(body.ID))
	body.BaseURL = strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	if !profileName.MatchString(body.ID) {
		writeError(w, 400, "the id is lowercase letters, digits, dot, dash and underscore", "invalid_request_error")
		return
	}
	if _, exists := r.specByID(body.ID); exists {
		writeError(w, 400, "a provider with this id exists", "invalid_request_error")
		return
	}
	if !strings.HasPrefix(body.BaseURL, "https://") && !strings.HasPrefix(body.BaseURL, "http://localhost") && !strings.HasPrefix(body.BaseURL, "http://127.0.0.1") {
		writeError(w, 400, "the address must be https:// (or http://localhost for a local server)", "invalid_request_error")
		return
	}
	if body.Discover == "" {
		body.Discover = "all"
	}
	if !slices.Contains([]string{"all", "free"}, body.Discover) {
		writeError(w, 400, "discover is all or free", "invalid_request_error")
		return
	}
	spec := ProviderSpec{
		ID: body.ID, BaseURL: body.BaseURL, TrainsOnFree: body.TrainsOnFree, Discover: body.Discover,
		DefaultTier: 3, SignupURL: body.SignupURL, NoKey: strings.TrimSpace(body.Key) == "",
	}
	if spec.NoKey {
		spec.KeyEnv = []string{} // nothing to send
	}
	key := strings.TrimSpace(body.Key)
	if key != "" {
		if _, err := r.checkKey(spec, key); err != nil {
			writeError(w, 400, err.Error(), "invalid_request_error")
			return
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := newProvider(spec, r.lookup).listModels(ctx, r.client); err != nil {
			writeError(w, 400, "the address did not answer GET /models: "+err.Error(), "invalid_request_error")
			return
		}
	}
	err := r.store.update(func(d *storeData) error {
		d.Custom = append(d.Custom, spec)
		if key != "" {
			if d.Keys == nil {
				d.Keys = map[string][]storedKey{}
			}
			d.Keys[spec.ID] = []storedKey{{ID: keyID(key), Key: key, Added: time.Now()}}
		}
		return nil
	})
	if err != nil {
		writeError(w, 500, err.Error(), "router_error")
		return
	}
	r.applyChange(spec.ID)
	writeJSON(w, map[string]any{"added": spec.ID})
}

// handleProviderDelete removes any provider. One added in the dashboard is
// gone with its keys; one from the seed stays in the list of providers to
// add, turned off, with the keys added in the dashboard dropped (keys from
// wend.env stay there, so such a provider is only turned off).
func (r *Router) handleProviderDelete(w http.ResponseWriter, req *http.Request) {
	if !r.needStore(w) {
		return
	}
	id := req.PathValue("id")
	if _, ok := r.specByID(id); !ok {
		writeError(w, 404, "no such provider", "invalid_request_error")
		return
	}
	err := r.store.update(func(d *storeData) error {
		n := len(d.Custom)
		d.Custom = slices.DeleteFunc(d.Custom, func(s ProviderSpec) bool { return s.ID == id })
		delete(d.Keys, id)
		delete(d.Enabled, id)
		if len(d.Custom) == n { // from the seed
			if d.Disabled == nil {
				d.Disabled = map[string]string{}
			}
			d.Disabled[id] = "removed in the dashboard " + time.Now().Format("2006-01-02")
		}
		return nil
	})
	switch {
	case err != nil:
		writeError(w, 500, err.Error(), "router_error")
	default:
		r.applyChange(id)
		writeJSON(w, map[string]any{"deleted": id})
	}
}
