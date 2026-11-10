package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Some providers say what the key itself has spent (OpenRouter: GET /key).
// That covers every user of the key, not only the router, so the dashboard
// shows it beside the router's own figure instead of treating a difference
// as an error.

type accountInfo struct {
	Provider       string   `json:"provider"`
	Today          float64  `json:"today"`
	Week           float64  `json:"week"`
	AllTime        float64  `json:"all_time"`
	Limit          *float64 `json:"limit,omitempty"`
	LimitRemaining *float64 `json:"limit_remaining,omitempty"`
	LimitReset     string   `json:"limit_reset,omitempty"`
	FetchedAt      string   `json:"fetched_at"`
}

func (p *Provider) fetchAccount(ctx context.Context, client *http.Client) (*accountInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", p.BaseURL+p.Spec.AccountPath, nil)
	if err != nil {
		return nil, err
	}
	p.authorize(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, &httpError{status: resp.StatusCode, body: truncate(string(body), 200)}
	}
	var d struct {
		Data struct {
			Usage          float64  `json:"usage"`
			UsageDaily     float64  `json:"usage_daily"`
			UsageWeekly    float64  `json:"usage_weekly"`
			Limit          *float64 `json:"limit"`
			LimitRemaining *float64 `json:"limit_remaining"`
			LimitReset     string   `json:"limit_reset"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &accountInfo{
		Provider: p.Spec.ID, Today: d.Data.UsageDaily, Week: d.Data.UsageWeekly, AllTime: d.Data.Usage,
		Limit: d.Data.Limit, LimitRemaining: d.Data.LimitRemaining, LimitReset: d.Data.LimitReset,
		FetchedAt: time.Now().Format(time.RFC3339),
	}, nil
}

// AccountLoop refreshes account spend for providers that report it.
func (r *Router) AccountLoop(ctx context.Context, every time.Duration) {
	refresh := func() {
		for _, p := range r.providerList() {
			if p.Spec.AccountPath == "" {
				continue
			}
			a, err := p.fetchAccount(ctx, r.client)
			if err != nil {
				continue
			}
			p.mu.Lock()
			p.account = a
			p.mu.Unlock()
		}
	}
	refresh()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

func (r *Router) accounts() []accountInfo {
	var out []accountInfo
	for _, p := range r.providerList() {
		p.mu.Lock()
		if p.account != nil {
			out = append(out, *p.account)
		}
		p.mu.Unlock()
	}
	return out
}
