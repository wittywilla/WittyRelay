// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

// Command uidemo serves the admin page over a made-up relay, for the browser
// test (e2e/) and for working on the page. It is never built into the image.
//
//	go run ./internal/admin/uidemo -listen 127.0.0.1:18081 -dir /tmp/uidemo
//
// The setup token is "demo-setup-token" unless -token says otherwise.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wittywilla/wittyrelay/internal/admin"
	"github.com/wittywilla/wittyrelay/internal/admin/fake"
	"github.com/wittywilla/wittyrelay/internal/relay"
)

// fixedClock is time.Now, or a set time while the history is filled.
type fixedClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fixedClock) set(t time.Time) { c.mu.Lock(); c.at = t; c.mu.Unlock() }

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() {
		return time.Now()
	}
	return c.at
}

var clock = &fixedClock{}

func main() {
	listen := flag.String("listen", "127.0.0.1:18081", "where to serve the page")
	dir := flag.String("dir", "", "data directory (a fresh temporary one when empty)")
	token := flag.String("token", "demo-setup-token", "the setup token")
	flag.Parse()
	if *dir == "" {
		d, err := os.MkdirTemp("", "uidemo-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		*dir = d
	}
	cfgPath := filepath.Join(*dir, "wittyrelay.json")
	_ = os.Setenv("WITTYRELAY_CONFIG", cfgPath)
	cfg := relay.Default()
	cfg.Name = "Example family relay"
	cfg.Note = "Run by Sam, for the family's servers."
	cfg.Keys = []string{"demo-access-key-never-shown-0001"}
	cfg.Announce = []string{"/dns4/relay.example.com/udp/4001/quic-v1", "/dns4/relay.example.com/tcp/4001"}
	cfg.DataDir = *dir
	cfg.Admin.Listen = *listen
	if _, err := os.Stat(cfgPath); err != nil {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		if err := os.MkdirAll(*dir, 0o700); err == nil {
			err = os.WriteFile(cfgPath, b, 0o600)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if c, err := relay.Load(); err == nil {
		cfg = c // what was saved on an earlier run
	}
	level := new(slog.LevelVar)
	ring := relay.NewLogRing(50)
	log := slog.New(relay.RingHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}), ring))
	log.Warn("registration refused", "reason", "key", "from", "203.0.113.9")
	log.Warn("admin sign-in: wrong password", "reason", "password")
	src := fake.New(cfg)
	src.Now = clock.now
	a, err := admin.New(src, admin.Options{Config: cfg, ConfigPath: cfgPath, DataDir: *dir, Ring: ring,
		Level: level, Log: log, SetupToken: *token, Now: clock.now})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Fill the graph's history at once, as if it had been up a while: the
	// clock walks through the last ten minutes, then runs as usual.
	for i := 119; i >= 0; i-- {
		clock.set(time.Now().Add(-time.Duration(i) * 5 * time.Second))
		a.Sample()
	}
	clock.set(time.Time{})
	go a.Run(context.Background())
	fmt.Println("uidemo on http://" + *listen + " (data in " + *dir + ")")
	if err := a.HTTPServer(*listen).ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
