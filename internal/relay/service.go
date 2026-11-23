// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

// Package relay is WittyRelay: a relay for WittyMediaServer's peer-to-peer
// connections, for the times a device and its server can't connect directly.
//
// It is libp2p's circuit relay v2, which WittyMediaServer and its apps
// already speak, so nothing new is needed on either end to be carried. What
// it adds is what a relay run for other people needs: servers register
// before they hold a place (with an access key, unless the relay is open),
// each server says which devices may reach it through the relay, every limit
// is said before use (ProtoInfo), and speed is limited per connection and in
// all. What it carries is encrypted end to end between the device and the
// server, with keys the relay never has: it can't read the media.
package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	libp2pquic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/libp2p/go-libp2p/x/rate"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// Version is set at build time.
var Version = "0.1.0-dev"

// UserAgent is what other nodes are told this is.
const UserAgent = "wittyrelay/1"

// Relay is a running relay.
type Relay struct {
	cfg   Config
	log   *slog.Logger
	host  host.Host
	relay *relayv2.Relay
	reg   *registry
	m     *metrics
	offer Offer

	ctx    context.Context
	cancel context.CancelFunc
}

// LoadKey reads the relay's identity from dir, making one the first time.
// The relay's ID, which servers pin, is the public half: keep the file.
func LoadKey(dir string) (crypto.PrivKey, error) {
	path := filepath.Join(dir, "identity.key")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(seed) + "\n"
		if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
			return nil, fmt.Errorf("keeping the relay's key in %s: %w", dir, err)
		}
		b = []byte(enc)
	} else if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s isn't a relay key", path)
	}
	return crypto.UnmarshalEd25519PrivateKey(ed25519.NewKeyFromSeed(seed))
}

// OfferOf is what a configuration allows, as servers are told it.
func OfferOf(cfg Config) Offer {
	l := cfg.Limits
	return Offer{V: 1, Software: "WittyRelay " + Version, Name: cfg.Name, Note: cfg.Note, Access: cfg.Access,
		Kbps: l.Kbps, TotalKbps: l.TotalKbps, MaxHeight: l.MaxHeight, DataMiB: l.DataMiB, Minutes: l.Minutes,
		Servers: l.Servers, Circuits: l.Circuits}
}

// Start starts the relay with key as its identity.
func Start(cfg Config, key crypto.PrivKey, log *slog.Logger) (*Relay, error) {
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := newMetrics()
	r := &Relay{cfg: cfg, log: log, m: m, offer: OfferOf(cfg), ctx: ctx, cancel: cancel}
	r.reg = newRegistry(&r.cfg, m)

	listen := cfg.Listen
	if len(listen) == 0 {
		listen = []string{
			fmt.Sprintf("/ip4/0.0.0.0/udp/%d/quic-v1", cfg.Port),
			fmt.Sprintf("/ip6/::/udp/%d/quic-v1", cfg.Port),
			fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", cfg.Port),
			fmt.Sprintf("/ip6/::/tcp/%d", cfg.Port),
		}
	}
	// Connections: every server's, its devices' and a margin. Servers and
	// devices in a circuit are protected by the relay itself.
	hi := connBudget(cfg)
	cm, err := connmgr.NewConnManager(hi*3/4, hi, connmgr.WithGracePeriod(time.Minute))
	if err != nil {
		cancel()
		return nil, err
	}
	rm, err := resources(cfg)
	if err != nil {
		cancel()
		return nil, err
	}
	opts := []libp2p.Option{
		libp2p.Identity(key),
		libp2p.UserAgent(UserAgent),
		libp2p.ListenAddrStrings(listen...),
		libp2p.Transport(libp2pquic.NewTransport),
		libp2p.Transport(tcp.NewTCPTransport),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Muxer(yamux.ID, yamux.DefaultTransport),
		libp2p.ConnectionManager(cm),
		libp2p.ResourceManager(rm),
		libp2p.DisableMetrics(),
		// A relay is somewhere everyone can reach, by definition; it doesn't
		// need to be told so, and must not go looking for relays of its own.
		libp2p.ForceReachabilityPublic(),
		libp2p.DisableRelay(),
	}
	if len(cfg.Announce) > 0 {
		var announce []ma.Multiaddr
		for _, a := range cfg.Announce {
			announce = append(announce, ma.StringCast(a))
		}
		opts = append(opts, libp2p.AddrsFactory(func([]ma.Multiaddr) []ma.Multiaddr { return announce }))
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		cancel()
		return nil, err
	}
	r.host = h

	res := relayv2.DefaultResources()
	res.Limit = &relayv2.RelayLimit{Duration: time.Duration(cfg.Limits.Minutes) * time.Minute, Data: cfg.Limits.DataMiB << 20}
	if cfg.Limits.Minutes == 0 && cfg.Limits.DataMiB == 0 {
		res.Limit = nil
	} else {
		if cfg.Limits.Minutes == 0 {
			res.Limit.Duration = 7 * 24 * time.Hour
		}
		if cfg.Limits.DataMiB == 0 {
			res.Limit.Data = 1 << 50
		}
	}
	res.MaxReservations = cfg.Limits.Servers
	res.MaxCircuits = cfg.Limits.Circuits
	res.MaxReservationsPerIP = cfg.Limits.PerIP
	res.MaxReservationsPerASN = max(res.MaxReservationsPerASN, cfg.Limits.PerIP)
	res.BufferSize = 16 << 10
	ph := &pacingHost{Host: h, ctx: ctx, kbps: cfg.Limits.Kbps, shared: newBucket(cfg.Limits.TotalKbps), m: m, tap: cfg.tap}
	ropts := []relayv2.Option{relayv2.WithResources(res), relayv2.WithACL(r.reg), relayv2.WithMetricsTracer(tracer{m})}
	if cfg.AllowPrivate {
		ropts = append(ropts, relayv2.WithReservationAddressFilter(func(ma.Multiaddr) bool { return true }))
	} else {
		ropts = append(ropts, relayv2.WithReservationAddressFilter(manet.IsPublicAddr))
	}
	rl, err := relayv2.New(ph, ropts...)
	if err != nil {
		_ = h.Close()
		cancel()
		return nil, err
	}
	r.relay = rl

	h.SetStreamHandler(ProtoRelay, func(s network.Stream) { _ = s.Close() })
	h.SetStreamHandler(ProtoInfo, r.handleInfo)
	h.SetStreamHandler(ProtoRegister, r.handleRegister)
	go r.expire()

	log.Info("WittyRelay started", "version", Version, "id", h.ID().String(), "access", cfg.Access,
		"kbps", cfg.Limits.Kbps, "totalKbps", cfg.Limits.TotalKbps, "maxHeight", cfg.Limits.MaxHeight,
		"dataMiB", cfg.Limits.DataMiB, "minutes", cfg.Limits.Minutes, "servers", cfg.Limits.Servers, "circuits", cfg.Limits.Circuits)
	for _, a := range r.Addrs() {
		log.Info("relay address, for Settings → Remote Access on a server", "address", a)
	}
	if len(cfg.Announce) == 0 && !cfg.AllowPrivate {
		log.Warn("no public address is set (WITTYRELAY_ANNOUNCE); servers are told what this machine finds, which behind NAT or in Docker is usually a private one")
	}
	return r, nil
}

// connBudget is how many connections the relay holds at most: one for each
// server and each of its circuits, and a margin for servers registering.
func connBudget(cfg Config) int { return cfg.Limits.Servers*(cfg.Limits.Circuits+1) + 64 }

// resourceLimits are what the relay may use, whatever anyone sends it: memory
// from MemoryMiB, which is the container's to give (libp2p left to itself
// sizes this from the whole machine's memory, which in a container is far
// more than it may use), and connections and file descriptors for every
// server and circuit the configuration allows, and no more. What one address
// may hold is in perAddress.
func resourceLimits(cfg Config) rcmgr.ConcreteLimitConfig {
	l := rcmgr.DefaultLimits
	libp2p.SetDefaultServiceLimits(&l)
	mem := int64(cfg.Limits.MemoryMiB) << 20
	conns := connBudget(cfg)
	transient := max(64, conns/4)
	return rcmgr.PartialLimitConfig{
		System: rcmgr.ResourceLimits{
			Conns: rcmgr.LimitVal(conns), ConnsInbound: rcmgr.LimitVal(conns), ConnsOutbound: rcmgr.LimitVal(conns),
			FD: rcmgr.LimitVal(conns + 64), Memory: rcmgr.LimitVal64(mem),
		},
		// Connections that haven't finished saying who they are yet: a slow or
		// silent stranger holds one of these, and only these, until it times out.
		Transient: rcmgr.ResourceLimits{
			Conns: rcmgr.LimitVal(transient), ConnsInbound: rcmgr.LimitVal(transient), ConnsOutbound: rcmgr.LimitVal(transient),
			FD: rcmgr.LimitVal(transient), Memory: rcmgr.LimitVal64(mem / 8),
		},
	}.Build(l.Scale(mem, conns+64))
}

// perAddress is how many connections one address may hold at once (an IPv4
// address, or an IPv6 /56): its servers' share, each with its circuits, so a
// household or an office behind one address gets what WITTYRELAY_PER_IP
// says, and never fewer than libp2p's own eight.
func perAddress(cfg Config) int { return max(8, cfg.Limits.PerIP*(cfg.Limits.Circuits+1)) }

func resources(cfg Config) (network.ResourceManager, error) {
	n := perAddress(cfg)
	// New connections from one address: a burst of that many, then two a second.
	pace := rate.Limit{RPS: 2, Burst: n}
	return rcmgr.NewResourceManager(rcmgr.NewFixedLimiter(resourceLimits(cfg)),
		rcmgr.WithLimitPerSubnet(
			[]rcmgr.ConnLimitPerSubnet{{PrefixLength: 32, ConnCount: n}},
			[]rcmgr.ConnLimitPerSubnet{{PrefixLength: 56, ConnCount: n}, {PrefixLength: 48, ConnCount: 4 * n}}),
		rcmgr.WithConnRateLimiters(&rate.Limiter{
			NetworkPrefixLimits: []rate.PrefixLimit{{Prefix: netip.MustParsePrefix("127.0.0.0/8")}, {Prefix: netip.MustParsePrefix("::1/128")}},
			SubnetRateLimiter: rate.SubnetLimiter{
				IPv4SubnetLimits: []rate.SubnetLimit{{PrefixLength: 32, Limit: pace}},
				IPv6SubnetLimits: []rate.SubnetLimit{{PrefixLength: 56, Limit: pace}},
				GracePeriod:      time.Minute,
			},
		}))
}

// ID is the relay's ID: what servers pin.
func (r *Relay) ID() string { return r.host.ID().String() }

// Host is the libp2p node, for tests.
func (r *Relay) Host() host.Host { return r.host }

// Addrs are the relay's addresses as servers should be given them.
func (r *Relay) Addrs() []string {
	var out []string
	for _, a := range r.host.Addrs() {
		if manet.IsIPLoopback(a) {
			continue
		}
		out = append(out, a.String()+"/p2p/"+r.ID())
	}
	return out
}

// Offer is what the relay allows.
func (r *Relay) Offer() Offer { return r.offer }

func (r *Relay) handleInfo(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(10 * time.Second))
	_ = writeMessage(s, r.offer, maxAnswer)
}

// registerWait is the most a registration may take, however slowly it is
// sent: a stream that trickles is cut off then.
const registerWait = 20 * time.Second

func (r *Relay) handleRegister(s network.Stream) {
	defer s.Close()
	wait := registerWait
	if r.cfg.registerWait > 0 {
		wait = r.cfg.registerWait
	}
	_ = s.SetDeadline(time.Now().Add(wait))
	server := s.Conn().RemotePeer()
	ip := ipOf(s.Conn().RemoteMultiaddr())
	var req RegisterRequest
	var res RegisterResponse
	if err := readMessage(s, &req, maxRegister); err != nil {
		r.m.refused("bad")
		res = RegisterResponse{Reason: "bad", Error: "That isn't a registration this relay understands."}
	} else {
		res = r.reg.register(server, ip, req)
	}
	res.Offer = r.offer
	if res.OK {
		r.m.registered.Add(1)
		r.log.Debug("server registered", "server", short(server.String()), "name", req.Name, "devices", len(req.Devices), "from", ip.String())
	} else {
		r.log.Info("registration refused", "server", short(server.String()), "reason", res.Reason, "from", ip.String())
	}
	_ = writeMessage(s, res, maxAnswer)
}

// expire forgets registrations nobody renewed, and cuts off their places.
func (r *Relay) expire() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			r.reg.count()
		}
	}
}

// Ready says the relay is listening.
func (r *Relay) Ready() bool { return len(r.host.Network().ListenAddresses()) > 0 }

// Close stops the relay.
func (r *Relay) Close() error {
	r.cancel()
	_ = r.relay.Close()
	return r.host.Close()
}

func short(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}
