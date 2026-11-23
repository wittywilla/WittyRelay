// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

// Package fake is a relay made up for the admin page's tests and its browser
// test: numbers that move, and no network.
package fake

import (
	"math"
	"sync"
	"time"

	"github.com/wittywilla/wittyrelay/internal/relay"
)

// Relay is a made-up relay.
type Relay struct {
	mu      sync.Mutex
	start   time.Time
	last    time.Time
	bytes   float64
	opened  int64
	Offer   relay.Offer
	Refused map[string]int64
	Now     func() time.Time // the clock; time.Now unless set
}

// New is a relay that has been up a while, carrying a few servers' streams.
func New(cfg relay.Config) *Relay {
	now := time.Now()
	return &Relay{start: now.Add(-(3*24*time.Hour + 4*time.Hour + 12*time.Minute)),
		bytes: 412 << 30, opened: 1843, Offer: relay.OfferOf(cfg), Now: time.Now,
		Refused: map[string]int64{"key": 12, "busy": 4, "connect": 3, "circuit-limit": 1}}
}

// Snapshot moves the numbers on as time passes: about 18 Mbit/s, rising and
// falling, and a device connection every few seconds.
func (f *Relay) Snapshot() relay.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.Now()
	dt := 0.0
	if !f.last.IsZero() && now.After(f.last) {
		dt = now.Sub(f.last).Seconds()
	}
	f.last = now
	t := float64(now.Unix())
	bps := 18e6 + 9e6*math.Sin(t/40) + 4e6*math.Sin(t/7)
	f.bytes += bps / 8 * dt
	f.opened += int64(dt / 4)
	reg := []relay.Registered{
		{ID: "srv-1c9e4a", Devices: 6, Until: now.Add(12 * time.Minute)},
		{ID: "srv-4f02d7", Devices: 3, Until: now.Add(9 * time.Minute)},
		{ID: "srv-8a6b31", Devices: 11, Until: now.Add(14 * time.Minute)},
		{ID: "srv-c27e90", Devices: 2, Until: now.Add(11 * time.Minute)},
		{ID: "srv-e5d418", Devices: 4, Until: now.Add(13 * time.Minute)},
	}
	refused := map[string]int64{}
	for k, v := range f.Refused {
		refused[k] = v
	}
	id := "12D3KooWRe1ayFakeDataForScreenshotsOnly7rX2mQ9vB4nT8kLp"
	return relay.Snapshot{Version: relay.Version, ID: id, Ready: true, Started: f.start, Port: 4001, Offer: f.Offer,
		Addrs:   []string{"/dns4/relay.example.com/udp/4001/quic-v1/p2p/" + id, "/dns4/relay.example.com/tcp/4001/p2p/" + id},
		Servers: 5, Reservations: 5, Circuits: 7, CircuitsTotal: f.opened, BytesTotal: int64(f.bytes), Registrations: 9120,
		Refused: refused, Registered: reg}
}
