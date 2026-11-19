// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"
)

// Everything here runs on 127.0.0.1: a relay, a "server" and "devices" that
// are plain libp2p nodes speaking what WittyMediaServer speaks.

const testKey = "a-test-access-key-0123456789"

func key(t *testing.T, b byte) crypto.PrivKey {
	t.Helper()
	k, err := crypto.UnmarshalEd25519PrivateKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testConfig() Config {
	c := Default()
	c.Listen = []string{"/ip4/127.0.0.1/tcp/0", "/ip4/127.0.0.1/udp/0/quic-v1"}
	c.HTTP = ""
	c.AllowPrivate = true
	c.Keys = []string{testKey}
	return c
}

func startRelay(t *testing.T, cfg Config) *Relay {
	t.Helper()
	r, err := Start(cfg, key(t, 1), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func node(t *testing.T, b byte) host.Host {
	t.Helper()
	h, err := libp2p.New(libp2p.Identity(key(t, b)), libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"),
		libp2p.DisableMetrics(), libp2p.EnableRelay())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func relayInfo(r *Relay) peer.AddrInfo {
	return peer.AddrInfo{ID: r.Host().ID(), Addrs: r.Host().Addrs()}
}

func register(t *testing.T, h host.Host, r *Relay, req RegisterRequest) RegisterResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.Connect(ctx, relayInfo(r)); err != nil {
		t.Fatal(err)
	}
	s, err := h.NewStream(ctx, r.Host().ID(), ProtoRegister)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := writeMessage(s, req, maxRegister); err != nil {
		t.Fatal(err)
	}
	var res RegisterResponse
	if err := readMessage(s, &res, maxAnswer); err != nil {
		t.Fatal(err)
	}
	return res
}

const echo protocol.ID = "/test/echo"

// serve makes h answer echo and hold a place on the relay.
func serve(t *testing.T, h host.Host, r *Relay) error {
	t.Helper()
	h.SetStreamHandler(echo, func(s network.Stream) {
		defer s.Close()
		_, _ = io.Copy(s, s)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := client.Reserve(ctx, h, relayInfo(r))
	return err
}

// through dials server via the relay and sends n bytes through and back.
func through(t *testing.T, dev host.Host, r *Relay, server peer.ID, n int) (time.Duration, error) {
	t.Helper()
	circuit := ma.StringCast(r.Host().Addrs()[0].String() + "/p2p/" + r.ID() + "/p2p-circuit")
	dev.Peerstore().AddAddrs(server, []ma.Multiaddr{circuit}, peerstore.TempAddrTTL)
	ctx, cancel := context.WithTimeout(network.WithAllowLimitedConn(context.Background(), "test"), 30*time.Second)
	defer cancel()
	start := time.Now()
	if err := dev.Connect(ctx, peer.AddrInfo{ID: server, Addrs: []ma.Multiaddr{circuit}}); err != nil {
		return 0, err
	}
	s, err := dev.NewStream(ctx, server, echo)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	go func() {
		_, _ = s.Write(bytes.Repeat([]byte("x"), n))
		_ = s.CloseWrite()
	}()
	got, err := io.ReadAll(s)
	if err != nil {
		return 0, err
	}
	if len(got) != n {
		return 0, io.ErrUnexpectedEOF
	}
	return time.Since(start), nil
}

func TestOnlyRegisteredServersAndTheirDevices(t *testing.T) {
	r := startRelay(t, testConfig())
	srv, dev, stranger := node(t, 2), node(t, 3), node(t, 4)

	// Not registered: no place.
	if err := serve(t, srv, r); err == nil {
		t.Fatal("a server that never registered was given a place")
	}
	// A wrong key, or none.
	if res := register(t, srv, r, RegisterRequest{V: 1, Key: "wrong-key-0000000000000", Devices: []string{dev.ID().String()}}); res.OK || res.Reason != "key" {
		t.Fatalf("wrong key: %+v", res)
	}
	if res := register(t, srv, r, RegisterRequest{V: 1, Devices: []string{dev.ID().String()}}); res.OK || res.Reason != "key" {
		t.Fatalf("no key: %+v", res)
	}
	// The right one, listing one device. The answer says what the relay allows.
	res := register(t, srv, r, RegisterRequest{V: 1, Key: testKey, Name: "Home", Devices: []string{dev.ID().String(), "not-an-id"}})
	if !res.OK || res.TTLSec != 900 || res.Offer.Kbps != 8000 || res.Offer.MaxHeight != 1080 || res.Offer.Access != "key" {
		t.Fatalf("register: %+v", res)
	}
	if err := serve(t, srv, r); err != nil {
		t.Fatalf("a registered server got no place: %v", err)
	}
	if _, err := through(t, dev, r, srv.ID(), 64<<10); err != nil {
		t.Fatalf("a listed device couldn't get through: %v", err)
	}
	if _, err := through(t, stranger, r, srv.ID(), 1024); err == nil {
		t.Fatal("a device the server didn't list got through")
	}
}

func TestOpenAccessStillNeedsRegistration(t *testing.T) {
	cfg := testConfig()
	cfg.Access, cfg.Keys = "open", nil
	r := startRelay(t, cfg)
	srv, dev := node(t, 2), node(t, 3)
	if err := serve(t, srv, r); err == nil {
		t.Fatal("an open relay gave a place without a registration")
	}
	if res := register(t, srv, r, RegisterRequest{V: 1, Devices: []string{dev.ID().String()}}); !res.OK || res.Offer.Access != "open" {
		t.Fatalf("%+v", res)
	}
	if err := serve(t, srv, r); err != nil {
		t.Fatal(err)
	}
	if _, err := through(t, dev, r, srv.ID(), 1024); err != nil {
		t.Fatal(err)
	}
}

func TestSpeedIsLimitedPerConnection(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.Kbps = 800 // 100 kB/s each way
	r := startRelay(t, cfg)
	srv, dev := node(t, 2), node(t, 3)
	register(t, srv, r, RegisterRequest{V: 1, Key: testKey, Devices: []string{dev.ID().String()}})
	if err := serve(t, srv, r); err != nil {
		t.Fatal(err)
	}
	// 300 kB there and back at 100 kB/s: over two seconds (a quarter second's
	// worth may go at once).
	d, err := through(t, dev, r, srv.ID(), 300<<10)
	if err != nil {
		t.Fatal(err)
	}
	if d < 2*time.Second {
		t.Fatalf("300 kB through a relay limited to 100 kB/s took %v", d)
	}
}

func TestRegistrationsAreLimited(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.Servers, cfg.Limits.RegisterPerMinute = 1, 3
	r := startRelay(t, cfg)
	a, b := node(t, 2), node(t, 3)
	if res := register(t, a, r, RegisterRequest{V: 1, Key: testKey}); !res.OK {
		t.Fatalf("%+v", res)
	}
	if res := register(t, b, r, RegisterRequest{V: 1, Key: testKey}); res.OK || res.Reason != "full" {
		t.Fatalf("a second server on a relay for one: %+v", res)
	}
	// Renewing is not a new server.
	if res := register(t, a, r, RegisterRequest{V: 1, Key: testKey}); !res.OK {
		t.Fatalf("renewal: %+v", res)
	}
	// All from 127.0.0.1: the address has used its three tries.
	if res := register(t, a, r, RegisterRequest{V: 1, Key: testKey}); res.OK || res.Reason != "busy" {
		t.Fatalf("fourth try in a minute: %+v", res)
	}
	// Too many devices listed.
	cfg2 := testConfig()
	cfg2.Limits.DevicesPerServer = 1
	r2 := startRelay(t, cfg2)
	c := node(t, 5)
	if res := register(t, c, r2, RegisterRequest{V: 1, Key: testKey, Devices: []string{a.ID().String(), b.ID().String()}}); res.OK || res.Reason != "bad" {
		t.Fatalf("%+v", res)
	}
}

func TestRegistrationsExpire(t *testing.T) {
	m := newMetrics()
	cfg := testConfig()
	reg := newRegistry(&cfg, m)
	now := time.Unix(1_000_000, 0)
	reg.now = func() time.Time { return now }
	srv, dev := key(t, 2), key(t, 3)
	sid, _ := peer.IDFromPrivateKey(srv)
	did, _ := peer.IDFromPrivateKey(dev)
	if res := reg.register(sid, ipOf(ma.StringCast("/ip4/203.0.113.9/tcp/1")), RegisterRequest{V: 1, Key: testKey, Devices: []string{did.String()}}); !res.OK {
		t.Fatalf("%+v", res)
	}
	if !reg.AllowReserve(sid, nil) || !reg.AllowConnect(did, nil, sid) {
		t.Fatal("registered, and refused")
	}
	if reg.AllowConnect(sid, nil, did) {
		t.Fatal("the other way round is no registration")
	}
	now = now.Add(16 * time.Minute)
	if reg.AllowReserve(sid, nil) || reg.AllowConnect(did, nil, sid) {
		t.Fatal("a registration nobody renewed still counts")
	}
}

func TestInfoAndHTTP(t *testing.T) {
	cfg := testConfig()
	cfg.Name, cfg.Note, cfg.MetricsToken = "Friends' relay", "Run by Sam", "tok"
	r := startRelay(t, cfg)
	h := node(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.Connect(ctx, relayInfo(r)); err != nil {
		t.Fatal(err)
	}
	s, err := h.NewStream(ctx, r.Host().ID(), ProtoInfo)
	if err != nil {
		t.Fatal(err)
	}
	var o Offer
	if err := readMessage(s, &o, maxAnswer); err != nil || o.Name != "Friends' relay" || o.Note != "Run by Sam" || o.Kbps != 8000 || !strings.HasPrefix(o.Software, "WittyRelay ") {
		t.Fatalf("%+v %v", o, err)
	}
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/metrics": 401, "/info": 200, "/nothing": 404} {
		res, err := http.Get(srv.URL + path)
		if err != nil || res.StatusCode != want {
			t.Fatalf("%s: %v %v", path, res.StatusCode, err)
		}
		res.Body.Close()
	}
	req, _ := http.NewRequest("GET", srv.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer tok")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), "wittyrelay_servers 0") {
		t.Fatalf("metrics: %s", b)
	}
}

func TestConfig(t *testing.T) {
	env := map[string]string{"WITTYRELAY_ACCESS": "open", "WITTYRELAY_KBPS": "2000", "WITTYRELAY_MAX_HEIGHT": "720",
		"WITTYRELAY_ANNOUNCE": "/dns4/relay.example.com/udp/4001/quic-v1, /dns4/relay.example.com/tcp/4001"}
	c := Default()
	if err := c.fromEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if err := c.Check(); err != nil {
		t.Fatal(err)
	}
	if c.Limits.Kbps != 2000 || c.Limits.MaxHeight != 720 || len(c.Announce) != 2 || c.Access != "open" {
		t.Fatalf("%+v", c)
	}
	bad := Default() // "key" with no keys
	if err := bad.Check(); err == nil || !strings.Contains(err.Error(), "wittyrelay key") {
		t.Fatalf("a key relay with no keys: %v", err)
	}
	c = Default()
	if err := c.fromEnv(func(k string) string {
		if k == "WITTYRELAY_KBPS" {
			return "fast"
		}
		return ""
	}); err == nil {
		t.Fatal("a speed that isn't a number was taken")
	}
}

func FuzzReadMessage(f *testing.F) {
	f.Add([]byte("\x00\x00\x00\x02{}"))
	f.Add([]byte("\x00\x00\x00\x10{\"v\":1,\"key\":\"x\"}"))
	f.Add([]byte("\xff\xff\xff\xff"))
	f.Fuzz(func(t *testing.T, b []byte) {
		var req RegisterRequest
		_ = readMessage(bytes.NewReader(b), &req, maxRegister)
		cfg := testConfig()
		reg := newRegistry(&cfg, newMetrics())
		id, _ := peer.IDFromPrivateKey(key(t, 9))
		res := reg.register(id, ipOf(ma.StringCast("/ip4/203.0.113.9/tcp/1")), req)
		if res.OK && req.Key != testKey {
			t.Fatal("registered without the key")
		}
	})
}
