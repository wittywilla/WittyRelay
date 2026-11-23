// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// The relay's health, as servers are told it, and the per-network counts the
// admin page shows.

func TestHealthIsToldWithTheOffer(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.Servers = 2
	r := startRelay(t, cfg)

	if h := r.Health(); h.State != "ok" || h.Load != 0 {
		t.Fatalf("an empty relay: %+v", h)
	}
	// Answered on relay-info, with every registration, and on /info.
	srv := node(t, 2)
	res := register(t, srv, r, RegisterRequest{V: 1, Key: testKey, Devices: []string{}})
	if !res.OK || res.Offer.Health == nil {
		t.Fatalf("registration: %+v", res)
	}
	// One server of two: half full (the answer may predate the count).
	r.hc.mu.Lock()
	r.hc.at = time.Time{}
	r.hc.mu.Unlock()
	if h := r.Health(); h.State != "ok" || h.Load != 50 {
		t.Fatalf("one server of two: %+v", h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := srv.NewStream(ctx, r.Host().ID(), ProtoInfo)
	if err != nil {
		t.Fatal(err)
	}
	var o Offer
	if err := readMessage(s, &o, maxAnswer); err != nil || o.Health == nil || o.Health.Load != 50 {
		t.Fatalf("relay-info: %+v %v", o, err)
	}
	hs := httptest.NewServer(r.Handler())
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/info")
	if err != nil {
		t.Fatal(err)
	}
	var info struct{ Offer Offer }
	_ = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info.Offer.Health == nil || info.Offer.Health.State != "ok" {
		t.Fatalf("/info: %+v", info.Offer)
	}
	// The static offer doesn't carry it: it changes.
	if OfferOf(cfg).Health != nil {
		t.Fatal("OfferOf has a health")
	}

	// Two of two: full.
	if res := register(t, node(t, 3), r, RegisterRequest{V: 1, Key: testKey, Devices: []string{}}); !res.OK {
		t.Fatalf("second server: %+v", res)
	}
	if h := r.health(0); h.State != "full" || h.Load != 100 {
		t.Fatalf("two servers of two: %+v", h)
	}
}

func TestHealthLoad(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.Servers, cfg.Limits.Circuits, cfg.Limits.TotalKbps = 10, 4, 1000
	r := startRelay(t, cfg)
	if h := r.health(0); h.State != "ok" || h.Load != 0 {
		t.Fatalf("idle: %+v", h)
	}
	// Device connections: 34 of 40 rounds to 90%.
	r.m.circuits.Store(34)
	if h := r.health(0); h.State != "busy" || h.Load != 90 {
		t.Fatalf("34 of 40 circuits: %+v", h)
	}
	r.m.circuits.Store(0)
	// Bandwidth: 1000 kbit/s each way is 2 Mbit/s both ways; 1.3 Mbit/s is 70%.
	r.bw.bps.Store(1_300_000)
	if h := r.health(0); h.State != "ok" || h.Load != 70 {
		t.Fatalf("bandwidth: %+v", h)
	}
	r.bw.bps.Store(0)
	// All connections, against the budget.
	if h := r.health(connBudget(cfg)); h.State != "busy" || h.Load != 100 {
		t.Fatalf("connections: %+v", h)
	}
}

func TestMeter(t *testing.T) {
	var m meter
	t0 := time.Unix(1000, 0)
	m.sample(0, t0)
	m.sample(1_000_000, t0.Add(time.Second)) // 8 Mbit/s, halved by smoothing
	if got := m.bps.Load(); got != 4_000_000 {
		t.Fatalf("bps %d", got)
	}
	m.sample(2_000_000, t0.Add(2*time.Second))
	if got := m.bps.Load(); got != 6_000_000 {
		t.Fatalf("bps %d", got)
	}
}

func TestNetworkUseCountsWithoutAddresses(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.PerIP = 2
	r := startRelay(t, cfg)
	// Two servers from 127.0.0.1: that network is at its limit.
	for i := byte(2); i < 4; i++ {
		if res := register(t, node(t, i), r, RegisterRequest{V: 1, Key: testKey, Devices: []string{}}); !res.OK {
			t.Fatalf("server %d: %+v", i, res)
		}
	}
	s := r.Snapshot()
	n := s.Networks
	if n.ServersMax != 2 || n.WithServers != 1 || n.BusiestServers != 2 || n.ServersAtMax != 1 {
		t.Fatalf("servers per network: %+v", n)
	}
	if n.ConnsMax != perAddress(cfg) || n.WithConns != 1 || n.BusiestConns < 2 || n.RegisterPerMinute != cfg.Limits.RegisterPerMinute {
		t.Fatalf("connections per address: %+v", n)
	}
	if s.Conns.Open < 2 || s.Conns.Inbound < 2 || s.Conns.Budget != connBudget(cfg) || s.Conns.MemoryMax != cfg.Limits.MemoryMiB {
		t.Fatalf("connections: %+v", s.Conns)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "127.0.0.1") {
		t.Fatalf("the snapshot shows an address: %s", b)
	}

	// Connections from an IPv6 /56 count as one address; servers from a /64.
	if connNetOf(netip.MustParseAddr("2001:db8:0:ff::1")) != connNetOf(netip.MustParseAddr("2001:db8:0:1::2")) {
		t.Fatal("one /56 counted as two")
	}
	if connNetOf(netip.MustParseAddr("2001:db8:0:100::1")) == connNetOf(netip.MustParseAddr("2001:db8:0:1::2")) {
		t.Fatal("two /56s counted as one")
	}
}

func TestMetricsHaveHealth(t *testing.T) {
	r := startRelay(t, testConfig())
	hs := httptest.NewServer(r.Handler())
	defer hs.Close()
	res, err := http.Get(hs.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	for _, want := range []string{"wittyrelay_load_percent 0", "wittyrelay_connections ", "wittyrelay_bandwidth_bits "} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("metrics lack %q: %s", want, b)
		}
	}
}
