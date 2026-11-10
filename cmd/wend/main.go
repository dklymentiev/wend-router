// wend: one local OpenAI-compatible endpoint in front of many LLM
// providers. It picks the strongest model that is up, and moves to the next
// one when a model fails, stalls or runs out of quota.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
)

// version is set at release builds with -ldflags "-X main.version=v0.1.0".
var version = "0.2.0" // a release build may set it with -ldflags "-X main.version=..."

const usage = `wend: one local endpoint for many LLM providers.

Usage:
  wend          start the router
  wend models   list the providers and models found, then exit
  wend version  print the version

Provider keys come from env (GROQ_API_KEY, OPENROUTER_API_KEY, NVIDIA_API_KEY,
SAMBANOVA_API_KEY, GEMINI_API_KEY, MISTRAL_API_KEY, CLOUDFLARE_API_TOKEN with
CLOUDFLARE_ACCOUNT_ID); a provider without a key is skipped. KEY=VALUE lines
in wend.env (next to the binary, or ROUTER_ENV_FILE) are read too;
variables already set win.

Settings (env):
  ROUTER_ADDR              listen address           (127.0.0.1:8888)
  ROUTER_SEED              seed file instead of the built-in one
  ROUTER_TTFT_TIMEOUT      first token deadline, at most (45s)
  ROUTER_TTFT_MIN          first token deadline, at least: a model gets 4x its usual wait in between (8s; 0 always waits the most)
  ROUTER_STALL_TIMEOUT     a model silent this long mid-answer is given up (45s; 0 no limit)
  ROUTER_REQUEST_TIMEOUT   non-streaming deadline   (180s)
  ROUTER_MAX_ATTEMPTS      models tried per request (6)
  ROUTER_ALLOW_TRAINING    0 skips providers whose free tier may train on prompts (1)
  ROUTER_TOKEN             require this bearer token from clients
  ROUTER_MAX_COST          USD a paid model may cost per request (0: free models only)
  ROUTER_DAILY_BUDGET      USD a day across paid models (1)
  ROUTER_REFRESH           model list refresh period (24h)
  ROUTER_STATE             state file (router-state.json next to the binary)
  ROUTER_KEYS              encrypted key store for keys added in the dashboard (keys.dat)
  ROUTER_PROBE             0 turns off the background checks of idle models (on)
  ROUTER_PROBE_EVERY       how often an idle model is re-checked (30m)
  ROUTER_MAX_WAIT          a request waits this long for a model on a short pause (90s)
  ROUTER_TEST              0 turns off background model tests (on; 8 calls a model, weekly)
  ROUTER_TRAFFIC_LOG       1 writes every request and response, as they were, to logs/ (off)
  ROUTER_TRAFFIC_DIR       where (logs next to the binary); ROUTER_TRAFFIC_DAYS days kept (7)
  ROUTER_PROFILES          profiles file (profiles.json next to the binary)
  ROUTER_FILTER            filter term list (filter.json next to the binary)
  ROUTER_ENTITIES          kinds of values the filter knows (entities.json next to the binary; written on first start)
  ROUTER_MASTER_KEY_FILE   key that encrypts keys.dat, outside Windows (in the user config folder)
  ROUTER_REMOTE_UI         1 accepts dashboard changes from other machines (this machine only)
  ROUTER_REMOTE_PIPE       1 shows the Pipe to other machines (this machine only)

Agent side:
  OPENAI_BASE_URL=http://localhost:8888/v1   model: <profile> | <provider/model>
  ANTHROPIC_BASE_URL=http://localhost:8888   (Messages API; an unknown claude-* model name
                                              uses ROUTER_ANTHROPIC_PROFILE, auto by default)
  Profiles (auto, fast, tools, vision, no-think, private, and your own) are
  edited on the dashboard and kept in profiles.json next to the binary
  (or ROUTER_PROFILES).
`

func main() {
	log.SetFlags(log.Ltime)
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "version", "--version":
		fmt.Println("wend", version)
		return
	case "", "serve", "models":
	default:
		fmt.Print(usage)
		if cmd != "-h" && cmd != "--help" && cmd != "help" {
			os.Exit(2)
		}
		return
	}

	if f := loadEnvFile(); f != "" {
		log.Printf("settings from %s", f)
	}
	seed, err := loadSeed(os.Getenv("ROUTER_SEED"))
	if err != nil {
		log.Fatal(err)
	}
	cfg := Config{
		TTFTTimeout:    envDuration("ROUTER_TTFT_TIMEOUT", 45*time.Second),
		TTFTMin:        envDuration("ROUTER_TTFT_MIN", 8*time.Second),
		StallTimeout:   envDuration("ROUTER_STALL_TIMEOUT", 45*time.Second),
		RequestTimeout: envDuration("ROUTER_REQUEST_TIMEOUT", 180*time.Second),
		MaxAttempts:    envInt("ROUTER_MAX_ATTEMPTS", 6),
		AllowTraining:  envOr("ROUTER_ALLOW_TRAINING", "1") != "0",
		Token:          os.Getenv("ROUTER_TOKEN"),
		MaxCost:        envFloat("ROUTER_MAX_COST", 0),
		DailyBudget:    envFloat("ROUTER_DAILY_BUDGET", 1),
		ProbeEvery:     envDuration("ROUTER_PROBE_EVERY", 30*time.Minute),
		MaxWait:        envDuration("ROUTER_MAX_WAIT", 90*time.Second),
	}
	if cfg.MaxCost > 0 {
		log.Printf("paid models allowed: up to $%g a request, $%g a day", cfg.MaxCost, cfg.DailyBudget)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	r := NewRouter(seed, cfg, os.Getenv)
	if ks, err := openKeyStore(keyStorePath()); err != nil {
		// Not opened means not written either: the file stays as it is.
		log.Printf("key store: %v; keys added in the dashboard are unavailable", err)
	} else {
		r.store = ks
		r.reloadProviders()
	}
	if err := r.loadProfiles(); err != nil {
		log.Fatal(err)
	}
	if err := r.LoadState(); err != nil {
		log.Printf("state: %v (starting fresh)", err)
	}
	r.Discover(ctx)
	r.PrintTable(os.Stdout)
	if cmd == "models" {
		return
	}
	go r.RefreshLoop(ctx, envDuration("ROUTER_REFRESH", 24*time.Hour))
	go r.StatusLoop(ctx.Done())
	go r.AccountLoop(ctx, 5*time.Minute)
	if os.Getenv("ROUTER_PROBE") != "0" {
		go r.ProbeLoop(ctx)
	}
	if os.Getenv("ROUTER_TEST") != "0" {
		go r.TestLoop(ctx)
	}
	stateDone := make(chan struct{})
	go func() { r.StateLoop(ctx.Done(), 30*time.Second); close(stateDone) }()

	addr := envOr("ROUTER_ADDR", "127.0.0.1:8888")
	srv := &http.Server{Addr: addr, Handler: r.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Printf("wend %s listening on http://%s  (dashboard: http://%s/dashboard)", version, addr, addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-stateDone // the last save on the way out
}

func envDuration(name string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil && d > 0 {
		return d
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if f, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64); err == nil && f >= 0 {
		return f
	}
	return def
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}
