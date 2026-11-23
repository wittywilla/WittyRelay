// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"fmt"
	"io"
	"math"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
)

// Health is how the relay is doing, told to servers with its offer (on
// relay-info, with every registration, and on /info) so a server's remote
// access screen can show it. It is coarse on purpose: anyone who can reach
// the relay may ask, so it says how full the relay is and nothing about who
// uses it.
type Health struct {
	// State: ok, busy (most of what it allows is in use: expect it to be
	// slower), full (it takes no more servers; those registered keep their
	// place) or starting.
	State string `json:"state"`
	// Load is the fullest of servers, device connections, all connections
	// and bandwidth, as a percentage rounded to the nearest 10.
	Load int `json:"load"`
}

// busyAt is the load from which a relay says it is busy.
const busyAt = 80

// ConnUse is every connection the relay holds, servers' and devices' alike,
// against what its configuration allows (resourceLimits).
type ConnUse struct {
	Open, Inbound, Outbound int
	Budget                  int // the most it holds at once (connBudget)
	// Unidentified are connections that haven't said who they are yet, and
	// the most it holds of them.
	Unidentified, UnidentifiedMax int
	// MemoryMiB is what libp2p has reserved for connections and streams, of
	// MemoryMax (WITTYRELAY_MEMORY_MIB).
	MemoryMiB float64
	MemoryMax int
}

// NetworkUse is the limits for one network or one address, and how near the
// busiest one is to them. The counts are made here, from addresses that never
// leave this file: the admin page and the log get numbers only.
type NetworkUse struct {
	// Servers from one network (an IPv4 address or an IPv6 /64):
	// WITTYRELAY_PER_IP.
	ServersMax     int
	WithServers    int // networks with a server registered now
	BusiestServers int // servers from the network with the most
	ServersAtMax   int // networks that can register no more

	// Connections from one address (an IPv4 address or an IPv6 /56): its
	// servers' share, each with its circuits (perAddress).
	ConnsMax     int
	WithConns    int
	BusiestConns int
	ConnsAtMax   int

	// RegisterPerMinute is how many registrations one network may make a
	// minute; NewConnBurst and NewConnPerSec pace new connections from one
	// address.
	RegisterPerMinute int
	NewConnBurst      int
	NewConnPerSec     int
}

// connNetOf is the address a connection counts as for the resource manager's
// per-address limit (resources): an IPv4 address, or an IPv6 /56.
func connNetOf(ip netip.Addr) netip.Prefix {
	if ip.Is4() || !ip.IsValid() {
		p, _ := ip.Prefix(ip.BitLen())
		return p
	}
	p, _ := ip.Prefix(56)
	return p
}

// newConnPerSec is how many new connections a second one address may make
// once its burst is spent (resources).
const newConnPerSec = 2

// meter is bandwidth now, sampled from the bytes counter.
type meter struct {
	bps  atomic.Int64 // bits a second, both ways, smoothed
	last int64
	at   time.Time
}

func (t *meter) sample(total int64, now time.Time) {
	if !t.at.IsZero() {
		if dt := now.Sub(t.at).Seconds(); dt > 0 {
			bps := float64(total-t.last) * 8 / dt
			prev := float64(t.bps.Load())
			t.bps.Store(int64(prev/2 + bps/2))
		}
	}
	t.last, t.at = total, now
}

// healthCache keeps Health for a moment: relay-info is answered to anyone,
// and counting connections for every ask would let a stranger make the relay
// do that work as fast as they can ask.
type healthCache struct {
	mu sync.Mutex
	h  Health
	at time.Time
}

const healthFor = 2 * time.Second

// Health is how the relay is doing now.
func (r *Relay) Health() Health {
	r.hc.mu.Lock()
	defer r.hc.mu.Unlock()
	if now := time.Now(); r.hc.at.IsZero() || now.Sub(r.hc.at) >= healthFor {
		r.hc.h, r.hc.at = r.health(len(r.host.Network().Conns())), now
	}
	return r.hc.h
}

func (r *Relay) health(conns int) Health {
	l := r.cfg.Limits
	servers := r.m.servers.Load()
	load := 0.0
	part := func(n, of float64) {
		if of > 0 {
			load = max(load, n/of)
		}
	}
	part(float64(servers), float64(l.Servers))
	part(float64(r.m.circuits.Load()), float64(l.Servers*l.Circuits))
	part(float64(conns), float64(connBudget(r.cfg)))
	// Bytes are counted both ways together; the cap holds each way.
	part(float64(r.bw.bps.Load()), 2*float64(l.TotalKbps)*1000)
	h := Health{State: "ok", Load: min(100, int(math.Round(load*10))*10)}
	switch {
	case !r.Ready():
		h.State = "starting"
	case l.Servers > 0 && servers >= int64(l.Servers):
		h.State = "full"
	case h.Load >= busyAt:
		h.State = "busy"
	}
	return h
}

// currentOffer is the offer with the relay's health now.
func (r *Relay) currentOffer() Offer {
	o := r.offer
	h := r.Health()
	o.Health = &h
	return o
}

// connUse counts the relay's connections, and how many each address holds.
func (r *Relay) connUse() (ConnUse, map[netip.Prefix]int) {
	u := ConnUse{Budget: connBudget(r.cfg), UnidentifiedMax: max(64, connBudget(r.cfg)/4), MemoryMax: r.cfg.Limits.MemoryMiB}
	per := map[netip.Prefix]int{}
	for _, c := range r.host.Network().Conns() {
		u.Open++
		if c.Stat().Direction == network.DirOutbound {
			u.Outbound++
		} else {
			u.Inbound++
		}
		per[connNetOf(ipOf(c.RemoteMultiaddr()))]++
	}
	if r.rm != nil {
		_ = r.rm.ViewTransient(func(s network.ResourceScope) error {
			st := s.Stat()
			u.Unidentified = st.NumConnsInbound + st.NumConnsOutbound
			return nil
		})
		_ = r.rm.ViewSystem(func(s network.ResourceScope) error {
			u.MemoryMiB = float64(s.Stat().Memory) / (1 << 20)
			return nil
		})
	}
	return u, per
}

// networkUse is how near the busiest network and address are to their limits.
func (r *Relay) networkUse(perConn map[netip.Prefix]int) NetworkUse {
	l := r.cfg.Limits
	n := perAddress(r.cfg)
	u := NetworkUse{ServersMax: l.PerIP, ConnsMax: n, RegisterPerMinute: l.RegisterPerMinute,
		NewConnBurst: n, NewConnPerSec: newConnPerSec}
	perServer := map[netip.Prefix]int{}
	r.reg.mu.Lock()
	now := r.reg.now()
	for _, s := range r.reg.servers {
		if now.Before(s.until) {
			perServer[netOf(s.ip)]++
		}
	}
	r.reg.mu.Unlock()
	u.WithServers, u.BusiestServers, u.ServersAtMax = busiest(perServer, l.PerIP)
	u.WithConns, u.BusiestConns, u.ConnsAtMax = busiest(perConn, n)
	return u
}

// busiest says how many keys there are, the most any has, and how many are
// at limit.
func busiest(m map[netip.Prefix]int, limit int) (keys, most, atLimit int) {
	for _, v := range m {
		most = max(most, v)
		if limit > 0 && v >= limit {
			atLimit++
		}
	}
	return len(m), most, atLimit
}

// writeHealth adds the health and connection counts to /metrics.
func (r *Relay) writeHealth(w io.Writer) {
	h := r.Health()
	u, _ := r.connUse()
	fmt.Fprintf(w, "# HELP wittyrelay_load_percent The fullest of servers, connections and bandwidth, rounded to 10.\n# TYPE wittyrelay_load_percent gauge\nwittyrelay_load_percent %d\n", h.Load)
	fmt.Fprintf(w, "# HELP wittyrelay_connections Connections open now, servers' and devices'.\n# TYPE wittyrelay_connections gauge\nwittyrelay_connections %d\n", u.Open)
	fmt.Fprintf(w, "# HELP wittyrelay_bandwidth_bits Bandwidth now, bits a second, both ways.\n# TYPE wittyrelay_bandwidth_bits gauge\nwittyrelay_bandwidth_bits %d\n", r.bw.bps.Load())
}
