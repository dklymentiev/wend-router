package main

import (
	"strings"
	"testing"
)

// One check per setting of the profile panel, in the panel's order: what the
// setting is for, and that changing it changes which models the profile takes
// or how it serves. Four providers p1..p4 each serve a model "m" at tiers 1..4.

func fieldsRouter(t *testing.T) (*Router, func(p *Profile, want ...string)) {
	t.Helper()
	fs := []*fake{newFake(t, okJSON("1")), newFake(t, okJSON("2")), newFake(t, okJSON("3")), newFake(t, okJSON("4"))}
	r, _ := newTestRouter(t, fs...)
	check := func(p *Profile, want ...string) {
		t.Helper()
		r.profiles["x"] = p
		got := keys(r.candidates(&reqInfo{model: "x", outTokens: 1000}))
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%+v: takes %v, want %v", *p, got, want)
		}
	}
	return r, check
}

func TestPanelToolCalling(t *testing.T) {
	r, check := fieldsRouter(t)
	r.models[0].tools, r.models[1].tools, r.models[2].tools = capYes, capNo, capUnknown
	check(&Profile{}, "p1/m", "p2/m", "p3/m", "p4/m")                   // Any
	check(&Profile{Require: []string{"tools"}}, "p1/m")                 // Required: known to have it
	check(&Profile{Exclude: []string{"tools"}}, "p2/m", "p3/m", "p4/m") // Excluded: unknown stays in
}

func TestPanelVision(t *testing.T) {
	r, check := fieldsRouter(t)
	r.models[1].vision = capYes
	check(&Profile{Require: []string{"vision"}}, "p2/m")
	check(&Profile{Exclude: []string{"vision"}}, "p1/m", "p3/m", "p4/m")
	// The old "Good for: vision" means the same.
	check(&Profile{Domains: []string{"vision"}}, "p2/m")
}

func TestPanelThinking(t *testing.T) {
	r, check := fieldsRouter(t)
	r.models[3].reasoning = capYes
	check(&Profile{Require: []string{"reasoning"}}, "p4/m")
	check(&Profile{Exclude: []string{"reasoning"}}, "p1/m", "p2/m", "p3/m")
}

func TestPanelProviders(t *testing.T) {
	_, check := fieldsRouter(t)
	check(&Profile{Providers: []string{"p3", "p1"}}, "p1/m", "p3/m")
	check(&Profile{}, "p1/m", "p2/m", "p3/m", "p4/m") // none ticked: all
}

func TestPanelNameContains(t *testing.T) {
	_, check := fieldsRouter(t)
	check(&Profile{Models: []string{"*p2*"}}, "p2/m")                 // across the slash
	check(&Profile{Models: []string{"*p2*", "*P4*"}}, "p2/m", "p4/m") // any of the words, any case
	check(&Profile{Models: []string{"*nothing*"}})
}

func TestPanelGoodFor(t *testing.T) {
	r, check := fieldsRouter(t)
	r.models[0].tags = []string{"code"}
	r.models[0].tools = capYes          // agents, untested: from tool calling
	r.models[1].tags = []string{"code"} // code only
	r.models[2].context = longContext   // long
	r.models[3].tps = fastTPS           // fast
	check(&Profile{Domains: []string{"code"}}, "p1/m", "p2/m")
	check(&Profile{Domains: []string{"code", "agents"}}, "p1/m") // every ticked area
	check(&Profile{Domains: []string{"long"}}, "p3/m")
	check(&Profile{Domains: []string{"fast"}}, "p4/m")
}

func TestPanelOrder(t *testing.T) {
	r, check := fieldsRouter(t)
	for i, ms := range []float64{900, 300, 600, 100} {
		r.models[i].ttftMs = ms
	}
	check(&Profile{}, "p1/m", "p2/m", "p3/m", "p4/m")                // strongest: tier 1 first
	check(&Profile{Sort: "fastest"}, "p4/m", "p2/m", "p3/m", "p1/m") // quickest first token first
	r.cfg.MaxCost, r.cfg.DailyBudget = 1, 10
	priced(r, "p1/m", 2, 2)
	priced(r, "p2/m", 1, 1)
	check(&Profile{Sort: "cheapest"}, "p3/m", "p4/m", "p2/m", "p1/m") // free, then the cheaper paid one
	r.cfg.MaxCost = 0
	priced(r, "p1/m", 0, 0)
	priced(r, "p2/m", 0, 0)
	for i, good := range []bool{false, true, true, true} {
		m := r.models[i]
		for range 10 {
			m.history[m.histN%historySize] = good
			m.histN++
		}
	}
	check(&Profile{Sort: "available"}, "p2/m", "p3/m", "p4/m", "p1/m") // the one that keeps failing last
}

func TestPanelStrength(t *testing.T) {
	_, check := fieldsRouter(t)
	check(&Profile{MaxTier: 1}, "p1/m")
	check(&Profile{MaxTier: 2}, "p1/m", "p2/m")
}

func TestPanelFirstTokenAndSpeed(t *testing.T) {
	r, check := fieldsRouter(t)
	r.models[0].ttftMs, r.models[0].tps = 400, 120
	r.models[1].ttftMs, r.models[1].tps = 1500, 30
	// p3, p4 never measured: left out by either limit
	check(&Profile{MaxTTFTMs: 1000}, "p1/m")
	check(&Profile{MaxTTFTMs: 2000}, "p1/m", "p2/m")
	check(&Profile{MinTPS: 50}, "p1/m")
	check(&Profile{MinTPS: 25}, "p1/m", "p2/m")
}

func TestPanelContextWindow(t *testing.T) {
	r, check := fieldsRouter(t)
	r.models[0].context, r.models[1].context, r.models[2].context = 32000, 128000, 1000000
	check(&Profile{MinContext: 128000}, "p2/m", "p3/m") // p4: unknown context, left out
	check(&Profile{MinContext: 1000000}, "p3/m")
}

// Ask the next after: TestHedgeRacesASlowModel, TestHedgeLeavesAFastModelAlone,
// TestHedgeFallsBackWhenTheFirstFailsEarly, TestHedgeBothSlowFirstAnswerWins.

func TestPanelPrice(t *testing.T) {
	r, check := fieldsRouter(t)
	priced(r, "p1/m", 0.1, 0.4) // $0.0005 for 1000 out
	priced(r, "p2/m", 3, 15)    // $0.015
	cap := 0.001
	check(&Profile{FreeOnly: true}, "p3/m", "p4/m")
	check(&Profile{}, "p3/m", "p4/m") // Default follows the router, which allows none
	r.cfg.MaxCost, r.cfg.DailyBudget = 1, 10
	check(&Profile{}, "p3/m", "p4/m", "p1/m", "p2/m") // Default with paid models allowed
	check(&Profile{MaxCost: &cap}, "p3/m", "p4/m", "p1/m")
}

func TestPanelNoTraining(t *testing.T) {
	r, check := fieldsRouter(t)
	r.models[1].trains = true
	check(&Profile{NoTraining: true}, "p1/m", "p3/m", "p4/m")
}

func TestPanelOnlyPinned(t *testing.T) {
	_, check := fieldsRouter(t)
	check(&Profile{Pinned: []string{"p3/m"}}, "p3/m", "p1/m", "p2/m", "p4/m")           // star: first
	check(&Profile{Pinned: []string{"p3/m", "p2/m"}, OnlyPinned: true}, "p3/m", "p2/m") // only the starred
	check(&Profile{Fallback: []string{"p1/m"}}, "p2/m", "p3/m", "p4/m", "p1/m")         // arrow: last
}
