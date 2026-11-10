package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed seed.json
var embeddedSeed []byte

// Seed is the router's own list of providers: endpoints, key env vars,
// documented free-tier facts and preferred models. Everything else about a
// model is discovered from the provider or measured at runtime.
type Seed struct {
	Providers []ProviderSpec `json:"providers"`
}

type ProviderSpec struct {
	ID           string      `json:"id"`
	Name         string      `json:"name,omitempty"`     // shown on the dashboard; the id when empty
	KeyNote      string      `json:"key_note,omitempty"` // what adding a key changes, for providers that work without one
	BaseURL      string      `json:"base_url"`
	BaseURLEnv   string      `json:"base_url_env,omitempty"`
	KeyEnv       []string    `json:"key_env,omitempty"`
	NoKey        bool        `json:"no_key,omitempty"`
	TrainsOnFree bool        `json:"trains_on_free"`
	Disabled     string      `json:"disabled,omitempty"`     // why the provider is off; skipped even with a key
	AccountPath  string      `json:"account_path,omitempty"` // GET path that reports the key's own spend
	SignupURL    string      `json:"signup_url,omitempty"`   // where to get a key
	Note         string      `json:"_note,omitempty"`
	Custom       bool        `json:"-"`                     // added in the dashboard
	ProbeEvery   string      `json:"probe_every,omitempty"` // how often an idle model is re-checked, e.g. "2h"
	TestManual   bool        `json:"test_manual,omitempty"` // tests only on request: the free quota is small
	Discover     string      `json:"discover"`              // "seed" | "seed-only" | "free" | "all"
	Exclude      []string    `json:"exclude,omitempty"`
	DefaultTier  int         `json:"default_tier,omitempty"`
	Models       []ModelSpec `json:"models,omitempty"`
}

type ModelSpec struct {
	ID      string   `json:"id"`
	Family  string   `json:"family,omitempty"` // the same model under another name elsewhere; see familyOf
	Tier    int      `json:"tier,omitempty"`
	Context int      `json:"context,omitempty"`
	Tools   *bool    `json:"tools,omitempty"`
	Vision  *bool    `json:"vision,omitempty"`
	Trains  *bool    `json:"trains,omitempty"`  // overrides the provider's trains_on_free
	Domains []string `json:"domains,omitempty"` // what it is good for: code, reasoning...
}

// loadSeed reads the seed from path, or the embedded copy when path is empty.
func loadSeed(path string) (*Seed, error) {
	data := embeddedSeed
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		data = b
	}
	var s Seed
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	return &s, nil
}
