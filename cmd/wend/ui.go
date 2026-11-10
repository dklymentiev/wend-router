package main

import (
	_ "embed"
	"net/http"
)

// The dashboard comes in two pages that share one stylesheet: Simple
// (/dashboard) for setting the router up and using it, Pro (/pro) with every
// table and setting. Each page has a switch to the other; the browser
// remembers the choice.

//go:embed ui/kit.css
var kitCSS []byte

//go:embed simple.html
var simpleHTML []byte

func (r *Router) handleKit(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache") // a new binary may bring a new kit
	_, _ = w.Write(kitCSS)
}

func (r *Router) handleSimple(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(simpleHTML)
}
