// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/netip"
	"sort"
	"time"
)

// Snapshot is the relay at one moment, for its admin page: counts and the
// relay's own public facts, never what it carries, who registered, or keys.
type Snapshot struct {
	Version string
	ID      string // the relay's own ID: public, servers pin it
	Ready   bool
	Started time.Time
	Addrs   []string
	Port    int
	Offer   Offer

	Servers       int64 // registered now
	Reservations  int64 // places held now
	Circuits      int64 // device connections open now
	CircuitsTotal int64 // device connections opened since start
	BytesTotal    int64 // bytes passed on, both ways, since start
	Registrations int64 // registrations accepted since start, renewals included
	Refused       map[string]int64

	Registered []Registered

	Health   Health     // what servers are told
	Conns    ConnUse    // every connection, against what is allowed
	Networks NetworkUse // per-network limits, and the busiest network
	BPS      int64      // bandwidth now, bits a second, both ways
}

// Registered is one registered server as the admin page shows it: a short ID
// that means nothing outside this run of the relay, and how many devices it
// listed. Not its name, its address or its peer ID.
type Registered struct {
	ID      string
	Devices int
	Until   time.Time
}

// anonSalt makes the short IDs: a fresh one every run, so they can't be
// matched to a server's peer ID or followed from one run to the next.
var anonSalt = func() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}()

func anonID(s string) string {
	h := hmac.New(sha256.New, anonSalt)
	h.Write([]byte(s))
	return "srv-" + hex.EncodeToString(h.Sum(nil)[:3])
}

// Snapshot reads the counters and the registry.
func (r *Relay) Snapshot() Snapshot {
	m := r.m
	s := Snapshot{Version: Version, ID: r.ID(), Ready: r.Ready(), Started: m.start, Addrs: r.Addrs(),
		Port: r.cfg.Port, Offer: r.offer, BPS: r.bw.bps.Load(),
		Servers: m.servers.Load(), Reservations: m.reservations.Load(), Circuits: m.circuits.Load(),
		CircuitsTotal: m.circuitsAll.Load(), BytesTotal: m.bytes.Load(), Registrations: m.registered.Load()}
	m.mu.Lock()
	s.Refused = maps.Clone(m.refusal)
	m.mu.Unlock()

	r.reg.mu.Lock()
	now := r.reg.now()
	for id, reg := range r.reg.servers {
		if now.After(reg.until) {
			continue
		}
		s.Registered = append(s.Registered, Registered{ID: anonID(id.String()), Devices: len(reg.devices), Until: reg.until})
	}
	r.reg.mu.Unlock()
	var per map[netip.Prefix]int
	s.Conns, per = r.connUse()
	s.Networks = r.networkUse(per)
	s.Health = r.health(s.Conns.Open)
	sort.Slice(s.Registered, func(i, j int) bool { return s.Registered[i].ID < s.Registered[j].ID })
	return s
}
