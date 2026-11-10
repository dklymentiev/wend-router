package main

import (
	"embed"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// The help of each dashboard page, for a person in the side panel and for an agent as
// plain Markdown. One text for both, so they never disagree.
//
//go:embed help/*.md
var helpFiles embed.FS

// helpPages in the order the index lists them.
var helpPages = []string{"start", "overview", "providers", "filter", "pipe"}

//go:embed ui/help.js
var helpJS []byte

// handleHelp serves /help (the index) and /help/{page}.md. The commands in the texts
// name http://127.0.0.1:8888; they are rewritten to the address the reader used, so
// what an agent copies runs against this router.
func (r *Router) handleHelp(w http.ResponseWriter, req *http.Request) {
	base := "http://" + req.Host
	if req.TLS != nil || req.Header.Get("X-Forwarded-Proto") == "https" {
		base = "https://" + req.Host
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	page := strings.TrimSuffix(req.PathValue("page"), ".md")
	if page == "" {
		var b strings.Builder
		b.WriteString("# Wend router help\n\nOne page for each part of the dashboard. An agent setting the router up can read\nthem all and use the API they describe.\n\n")
		for _, p := range helpPages {
			fmt.Fprintf(&b, "- [%s](%s/help/%s.md)\n", helpTitle(p), base, p)
		}
		_, _ = w.Write([]byte(b.String()))
		return
	}
	if !slices.Contains(helpPages, page) {
		writeError(w, 404, "no such help page; the list is at /help", "invalid_request_error")
		return
	}
	text, _ := helpFiles.ReadFile("help/" + page + ".md")
	_, _ = w.Write([]byte(strings.ReplaceAll(string(text), "http://127.0.0.1:8888", base)))
}

// helpTitle is the page's first heading.
func helpTitle(page string) string {
	text, _ := helpFiles.ReadFile("help/" + page + ".md")
	first, _, _ := strings.Cut(string(text), "\n")
	return strings.TrimPrefix(first, "# ")
}

func (r *Router) handleHelpJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache") // a new binary may bring new help
	_, _ = w.Write(helpJS)
}
