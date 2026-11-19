// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"
)

// Handler serves health, readiness, metrics and the offer over plain HTTP,
// for Docker's health check, a monitoring system, and owners setting up a
// server. Nothing secret is served: /info is what any server is told anyway.
//
//	GET /healthz  200 while the process runs
//	GET /readyz   200 once listening
//	GET /metrics  Prometheus text (a bearer token if MetricsToken is set)
//	GET /info     the offer and the relay's addresses, as JSON
func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !r.Ready() {
			http.Error(w, "not listening yet", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, req *http.Request) {
		if tok := r.cfg.MetricsToken; tok != "" {
			got := req.Header.Get("Authorization")
			if subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+tok)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.m.write(w)
	})
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": r.ID(), "addresses": r.Addrs(), "offer": r.offer})
	})
	return mux
}

// HTTPServer is Handler with timeouts fit for something on the internet.
func (r *Relay) HTTPServer(addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: r.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8 << 10}
}
