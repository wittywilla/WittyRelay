// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/netip"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// registration is one server that may hold a place, and the devices it said
// may reach it through this relay.
type registration struct {
	name    string
	ip      netip.Addr
	devices map[peer.ID]struct{}
	until   time.Time
}

// registry is who may use the relay. It is the relay's ACL: a server holds a
// place only while registered, and a device reaches a server only if that
// server listed it.
type registry struct {
	mu      sync.Mutex
	cfg     *Config
	keys    [][32]byte // hashes of the access keys, compared in constant time
	servers map[peer.ID]*registration
	tries   *window
	now     func() time.Time
	m       *metrics
}

func newRegistry(cfg *Config, m *metrics) *registry {
	r := &registry{cfg: cfg, servers: map[peer.ID]*registration{}, now: time.Now, m: m,
		tries: newWindow(cfg.Limits.RegisterPerMinute, time.Minute, 4096)}
	for _, k := range cfg.Keys {
		r.keys = append(r.keys, sha256.Sum256([]byte(k)))
	}
	return r
}

func (r *registry) keyOK(k string) bool {
	if r.cfg.Access == "open" {
		return true
	}
	h := sha256.Sum256([]byte(k))
	ok := 0
	for _, want := range r.keys {
		ok |= subtle.ConstantTimeCompare(h[:], want[:])
	}
	return ok == 1
}

// register answers a server's registration. ip is where it connected from.
func (r *registry) register(server peer.ID, ip netip.Addr, req RegisterRequest) RegisterResponse {
	refuse := func(reason, msg string) RegisterResponse {
		r.m.refused(reason)
		return RegisterResponse{Reason: reason, Error: msg}
	}
	// Counted per network before anything else is looked at: an IPv6 network
	// hands one home a great many addresses, so they count as one (netOf).
	// Per server only once the key is right, so a stranger making up server
	// IDs can't fill the table every server's renewals are counted in.
	if !r.tries.allow("ip:" + netOf(ip).String()) {
		return refuse("busy", "Too many registrations; try again in a minute.")
	}
	if req.V != 1 {
		return refuse("bad", "This relay speaks version 1 of the registration.")
	}
	if !r.keyOK(req.Key) {
		if req.Key == "" {
			return refuse("key", "This relay needs an access key from whoever runs it.")
		}
		return refuse("key", "That access key isn't one this relay knows.")
	}
	if !r.tries.allow("id:" + server.String()) {
		return refuse("busy", "Too many registrations; try again in a minute.")
	}
	if len(req.Devices) > r.cfg.Limits.DevicesPerServer {
		return refuse("bad", "Too many devices listed for this relay.")
	}
	devices := make(map[peer.ID]struct{}, len(req.Devices))
	for _, d := range req.Devices {
		id, err := peer.Decode(d)
		if err != nil {
			continue
		}
		if pub, err := id.ExtractPublicKey(); err != nil || pub.Type() != crypto.Ed25519 {
			continue
		}
		devices[id] = struct{}{}
	}
	name := req.Name
	if len(name) > 60 {
		name = name[:60]
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.expireLocked(now)
	if _, known := r.servers[server]; !known {
		if len(r.servers) >= r.cfg.Limits.Servers {
			return refuse("full", "This relay has as many servers as it takes.")
		}
		same := 0
		for _, s := range r.servers {
			if netOf(s.ip) == netOf(ip) {
				same++
			}
		}
		if same >= r.cfg.Limits.PerIP {
			return refuse("full", "This relay takes no more servers from your address.")
		}
	}
	ttl := time.Duration(r.cfg.RegisterTTL)
	r.servers[server] = &registration{name: name, ip: ip, devices: devices, until: now.Add(ttl)}
	r.m.servers.Store(int64(len(r.servers)))
	return RegisterResponse{OK: true, TTLSec: int(ttl / time.Second)}
}

func (r *registry) expireLocked(now time.Time) {
	for id, s := range r.servers {
		if now.After(s.until) {
			delete(r.servers, id)
		}
	}
	r.m.servers.Store(int64(len(r.servers)))
}

// count is how many servers are registered now.
func (r *registry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(r.now())
	return len(r.servers)
}

// AllowReserve: only a registered server holds a place.
func (r *registry) AllowReserve(p peer.ID, a ma.Multiaddr) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.servers[p]
	allowed := ok && r.now().Before(s.until)
	if !allowed {
		r.m.refused("reserve")
	}
	return allowed
}

// AllowConnect: a device reaches a server only if the server listed it.
func (r *registry) AllowConnect(src peer.ID, _ ma.Multiaddr, dest peer.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.servers[dest]
	if ok && r.now().Before(s.until) {
		if _, listed := s.devices[src]; listed {
			return true
		}
	}
	r.m.refused("connect")
	return false
}

// ipOf is the IP address a multiaddr starts with.
func ipOf(a ma.Multiaddr) netip.Addr {
	ip, err := manet.ToIP(a)
	if err != nil {
		return netip.Addr{}
	}
	out, _ := netip.AddrFromSlice(ip)
	return out.Unmap()
}

// netOf is the network an address counts as for limits: the address itself
// for IPv4, and its /64 for IPv6, the least one home or one server is given.
func netOf(ip netip.Addr) netip.Prefix {
	if ip.Is4() || !ip.IsValid() {
		p, _ := ip.Prefix(ip.BitLen())
		return p
	}
	p, _ := ip.Prefix(64)
	return p
}

// window allows so many tries per key in a stretch of time, for a bounded
// number of keys: when the table is full, new keys are refused, so filling it
// buys a stranger nothing.
type window struct {
	mu    sync.Mutex
	limit int
	span  time.Duration
	max   int
	tries map[string][]time.Time
	now   func() time.Time
}

func newWindow(limit int, span time.Duration, maxKeys int) *window {
	return &window{limit: limit, span: span, max: maxKeys, tries: map[string][]time.Time{}, now: time.Now}
}

func (w *window) allow(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if len(w.tries) >= w.max {
		for k, list := range w.tries {
			if len(list) == 0 || now.Sub(list[len(list)-1]) >= w.span {
				delete(w.tries, k)
			}
		}
	}
	list, known := w.tries[key]
	if !known && len(w.tries) >= w.max {
		return false
	}
	keep := list[:0]
	for _, t := range list {
		if now.Sub(t) < w.span {
			keep = append(keep, t)
		}
	}
	if len(keep) >= w.limit {
		w.tries[key] = keep
		return false
	}
	w.tries[key] = append(keep, now)
	return true
}
