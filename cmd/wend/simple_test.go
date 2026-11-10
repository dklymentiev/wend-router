package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for what the simple dashboard relies on: models grouped by family,
// profiles that pick models and their providers, built-in profiles and
// providers that go like any other, keys for every provider, clients.

func TestFamilyOf(t *testing.T) {
	for _, c := range []struct{ id, seed, want string }{
		{"nvidia/Nemotron-3-Super:free", "", "nemotron-3-super"},
		{"gpt-oss-120b", "", "gpt-oss-120b"},
		{"openai/gpt-oss-120b", "", "gpt-oss-120b"},
		{"Meta-Llama-3_3-70B-Instruct", "llama-3.3-70b", "llama-3.3-70b"}, // the seed names what the id does not
		{"gpt-oss:120b", "", "gpt-oss-120b"},                              // a size is part of the model
		{"gpt-oss:20b", "", "gpt-oss-20b"},
		{"deepseek-v4-pro:0813", "", "deepseek-v4-pro"}, // a date or variant is not
	} {
		if got := familyOf(c.id, c.seed); got != c.want {
			t.Errorf("familyOf(%q, %q) = %q, want %q", c.id, c.seed, got, c.want)
		}
	}
}

func TestProfileFamiliesPickModelsAndProviders(t *testing.T) {
	a, b, c := newFake(t, okJSON("a")), newFake(t, okJSON("b")), newFake(t, okJSON("c"))
	r, _ := newTestRouter(t, a, b, c) // the same model "m" at p1, p2, p3
	check := func(p *Profile, want ...string) {
		t.Helper()
		r.profiles["x"] = p
		got := keys(r.candidates(&reqInfo{model: "x", outTokens: 1000}))
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%+v: got %v, want %v", p.Families, got, want)
		}
	}
	check(&Profile{Families: map[string][]string{"m": nil}}, "p1/m", "p2/m", "p3/m")              // any provider
	check(&Profile{Families: map[string][]string{"m": {"p3", "p2"}}}, "p2/m", "p3/m")             // chosen providers only
	check(&Profile{Families: map[string][]string{"other": nil}})                                  // a model nobody serves
	if err := (&Profile{Families: map[string][]string{"Nemotron": nil}}).validate(); err == nil { // as listed: lower case
		t.Fatal("a family in mixed case was accepted")
	}

	// The builder's list: one family, its three providers.
	fs := r.families()
	if len(fs) != 1 || fs[0].ID != "m" || len(fs[0].Models) != 3 {
		t.Fatalf("families: %+v", fs)
	}
}

func TestBuiltInProfileCanBeDeletedAndStaysDeleted(t *testing.T) {
	t.Setenv("ROUTER_PROFILES", filepath.Join(t.TempDir(), "profiles.json"))
	r, srv := newTestRouter(t, newFake(t, okJSON("a")))
	if code, body := uiCall(t, "DELETE", srv.URL+"/api/profiles/auto", ""); code != 200 {
		t.Fatalf("delete auto: %d %s", code, body)
	}
	r.profiles = nil
	if err := r.loadProfiles(); err != nil {
		t.Fatal(err)
	}
	if r.profile("auto") != nil {
		t.Fatal("a deleted built-in profile came back after a reload")
	}
	if r.profile("fast") == nil {
		t.Fatal("the other built-in profiles were lost")
	}
	// A name that is no longer a profile is a model nobody serves: an error, not a guess.
	if got := keys(r.candidates(&reqInfo{model: "auto", outTokens: 1000})); len(got) != 0 {
		t.Fatalf("auto after deletion: %v, want none", got)
	}
	if resp, body := post(t, srv, plain); resp.StatusCode != 404 || !strings.Contains(body, "model_not_found") {
		t.Fatalf("auto after deletion: %d %s, want 404 model_not_found", resp.StatusCode, body)
	}
}

func TestOlderProfilesFileIsLaidOverBuiltInsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	t.Setenv("ROUTER_PROFILES", path)
	if err := os.WriteFile(path, []byte(`{"coder":{"require":["tools"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, _ := newTestRouter(t, newFake(t, okJSON("a")))
	if err := r.loadProfiles(); err != nil {
		t.Fatal(err)
	}
	if r.profile("coder") == nil || r.profile("auto") == nil {
		t.Fatal("older file: its profile or the built-ins are missing")
	}
	var f profilesFile
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &f); err != nil || f.Version != 2 || f.Profiles["auto"] == nil || f.Profiles["coder"] == nil {
		t.Fatalf("older file not rewritten whole: %s", b)
	}
}

func TestEveryProviderTakesAKeyAndAnyCanBeRemoved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROUTER_MASTER_KEY_FILE", filepath.Join(dir, "master.key"))
	up := keyedFake(t, "paid-key-1234567")
	// "free" works without a key, like OVHcloud; a key is still accepted.
	seed := &Seed{Providers: []ProviderSpec{{ID: "free", Name: "Free Cloud", BaseURL: up.URL + "/v1", NoKey: true, Discover: "all",
		KeyNote: "With a key it bills per use."}}}
	r := NewRouter(seed, Config{MaxAttempts: 3, TTFTTimeout: time.Second, RequestTimeout: time.Second}, func(string) string { return "" })
	ks, err := openKeyStore(filepath.Join(dir, "keys.dat"))
	if err != nil {
		t.Fatal(err)
	}
	r.store = ks
	r.reloadProviders()
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)

	if code, body := uiCall(t, "POST", srv.URL+"/api/providers/free/keys", `{"key":"paid-key-1234567"}`); code != 200 {
		t.Fatalf("key for a provider that works without one: %d %s", code, body)
	}
	if p := r.providerByID("free"); p == nil || len(p.keyList()) != 1 {
		t.Fatal("the key is not used")
	}
	view := func() providerView {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/providers")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct{ Providers []providerView }
		b, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(b, &out); err != nil || len(out.Providers) != 1 {
			t.Fatalf("providers: %s", b)
		}
		return out.Providers[0]
	}
	if v := view(); !v.Listed || !v.On || v.Name != "Free Cloud" || v.KeyMode != "optional" || v.KeyNote == "" {
		t.Fatalf("view: %+v", v)
	}

	// Turned off with its switch it stays listed, off.
	if code, body := uiCall(t, "POST", srv.URL+"/api/providers/free/enabled", `{"enabled":false}`); code != 200 {
		t.Fatalf("switch off: %d %s", code, body)
	}
	if v := view(); !v.Listed || v.On {
		t.Fatalf("switched off: %+v", v)
	}
	// A provider from the seed can be removed: off, its dashboard keys gone, listed to add again.
	if code, body := uiCall(t, "DELETE", srv.URL+"/api/providers/free", ""); code != 200 {
		t.Fatalf("remove a seed provider: %d %s", code, body)
	}
	if r.providerByID("free") != nil || len(ks.view().Keys["free"]) != 0 {
		t.Fatal("removed provider still on, or its key kept")
	}
	if v := view(); v.Listed {
		t.Fatalf("removed provider shown as added: %+v", v)
	}
	// Adding a key brings it back.
	if code, body := uiCall(t, "POST", srv.URL+"/api/providers/free/keys", `{"key":"paid-key-1234567"}`); code != 200 {
		t.Fatalf("key after removal: %d %s", code, body)
	}
	if v := view(); !v.Listed || !v.On {
		t.Fatal("a key did not bring the provider back")
	}
}

func TestClientsSumSessionsByClient(t *testing.T) {
	b := newSessionBook()
	b.start("s1", "coder", "auto", true)
	b.finish("s1", "p1/m", nil, 10, 5)
	b.start("s2", "coder", "mix", true)
	b.start("s2", "coder", "mix", true)
	b.start("s3", "curl", "auto", true)
	cs := b.clients(time.Hour)
	if len(cs) != 2 {
		t.Fatalf("clients: %+v", cs)
	}
	var coder clientInfo
	for _, c := range cs {
		if c.Client == "coder" {
			coder = c
		}
	}
	if coder.Requests != 3 || coder.InFlight != 2 || len(coder.Profiles) != 2 || coder.Profiles[0] != "mix" {
		t.Fatalf("coder: %+v", coder)
	}
}

func TestAskedNamesAreRemembered(t *testing.T) {
	u := newUsageLedger()
	u.record(&reqInfo{model: "nemotron", client: "coder"}, "p1/m", "ok", nil)
	u.record(&reqInfo{model: "", client: "coder"}, "p1/m", "ok", nil)
	u.record(&reqInfo{model: "bench", probe: true}, "p1/m", "ok", nil)
	a := u.askedSnapshot()
	if a["nemotron"].IsZero() || a["auto"].IsZero() {
		t.Fatalf("asked names not kept: %v", a)
	}
	if _, ok := a["bench"]; ok {
		t.Fatal("a probe counted as a profile in use")
	}
	v := newUsageLedger()
	v.restoreAsked(a)
	if len(v.askedSnapshot()) != 2 {
		t.Fatal("asked names lost across a restart")
	}
}

func TestNameMatchesAcrossSlashes(t *testing.T) {
	for _, c := range []struct {
		pattern, key string
		want         bool
	}{
		{"*gpt*", "ovh/gpt-oss-20b", true},
		{"*nemotron*", "kilo/nvidia/nemotron-3.5-lightning:free", true},
		{"kilo/*", "kilo/nvidia/nemotron-3.5-lightning:free", true},
		{"*GPT*", "ovh/gpt-oss-20b", true},
		{"*qwen*", "ovh/gpt-oss-20b", false},
		{"ovh/*", "kilo/x", false},
	} {
		if got := nameMatches(c.pattern, c.key); got != c.want {
			t.Errorf("nameMatches(%q, %q) = %v, want %v", c.pattern, c.key, got, c.want)
		}
	}
}

func TestTestAllShowsProgressAndStops(t *testing.T) {
	old := testPause
	testPause = 30 * time.Millisecond
	t.Cleanup(func() { testPause = old })
	a, b := smartFake(t), smartFake(t)
	r, _ := newTestRouter(t, a, b)
	wait := func(cond func(v testRunView) bool) testRunView {
		t.Helper()
		for range 300 {
			if v := r.runView(); v != nil && cond(*v) {
				return *v
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("run never got there: %+v", r.runView())
		return testRunView{}
	}

	// A whole run: every model done, every case counted.
	if !r.startRun("test", r.models) {
		t.Fatal("run did not start")
	}
	if r.startRun("test", r.models) {
		t.Fatal("a second run started over the first")
	}
	v := wait(func(v testRunView) bool { return !v.Running })
	if v.Done != 2 || v.CallsDone != v.Calls || v.Calls != 2*len(testCases) || v.Stopped || len(v.Now) != 0 {
		t.Fatalf("finished run: %+v", v)
	}

	stop := func(q string) {
		t.Helper()
		w := httptest.NewRecorder()
		r.handleTestStop(w, httptest.NewRequest("POST", "/api/test/stop"+q, nil))
		if w.Code != 200 {
			t.Fatalf("stop%s: %d %s", q, w.Code, w.Body)
		}
	}

	// A check is a run too: one call a model.
	if !r.startRun("check", r.models) {
		t.Fatal("check did not start")
	}
	if v := wait(func(v testRunView) bool { return !v.Running }); v.Kind != "check" || v.Done != 2 || v.Calls != 2 || v.CallsDone != 2 {
		t.Fatalf("check: %+v", v)
	}

	// One model stopped: the other goes on and finishes.
	testPause = 300 * time.Millisecond
	r.startRun("test", r.models)
	wait(func(v testRunView) bool { return len(v.Now) == 2 })
	stop("?model=p1/m")
	v = wait(func(v testRunView) bool { return !v.Running })
	if v.Stopped || v.Done != 1 {
		t.Fatalf("one model stopped: %+v", v)
	}

	// The run stopped halfway: it ends at once, says so, and the models are not done.
	r.startRun("test", r.models)
	v = wait(func(v testRunView) bool { return v.Running && v.CallsDone > 0 && len(v.Now) == 2 })
	start := time.Now()
	stop("")
	v = wait(func(v testRunView) bool { return !v.Running })
	if !v.Stopped || v.Done != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("stopped run: %+v after %s", v, time.Since(start))
	}
}
