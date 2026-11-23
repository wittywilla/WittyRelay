// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	ma "github.com/multiformats/go-multiaddr"
)

// What a relay run for strangers must hold to, whatever they send it.

// The relay passes on bytes it can't read: a server's answer crosses it as ciphertext.
func TestTheRelaySeesOnlyCiphertext(t *testing.T) {
	const marker = "plaintext-marker-a41c77e0"
	var mu sync.Mutex
	var seen bytes.Buffer
	cfg := testConfig()
	cfg.tap = func(b []byte) {
		mu.Lock()
		seen.Write(b)
		mu.Unlock()
	}
	r := startRelay(t, cfg)
	server, dev := node(t, 2), node(t, 3)
	if res := register(t, server, r, RegisterRequest{V: 1, Key: testKey, Devices: []string{dev.ID().String()}}); !res.OK {
		t.Fatalf("%+v", res)
	}
	if err := serve(t, server, r); err != nil {
		t.Fatal(err)
	}
	circuit := ma.StringCast(r.Host().Addrs()[0].String() + "/p2p/" + r.ID() + "/p2p-circuit")
	dev.Peerstore().AddAddrs(server.ID(), []ma.Multiaddr{circuit}, peerstore.TempAddrTTL)
	ctx, cancel := context.WithTimeout(network.WithAllowLimitedConn(context.Background(), "test"), 30*time.Second)
	defer cancel()
	if err := dev.Connect(ctx, peer.AddrInfo{ID: server.ID(), Addrs: []ma.Multiaddr{circuit}}); err != nil {
		t.Fatal(err)
	}
	s, err := dev.NewStream(ctx, server.ID(), echo)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte(strings.Repeat(marker, 1000))
	go func() {
		_, _ = s.Write(msg)
		_ = s.CloseWrite()
	}()
	got, err := io.ReadAll(s)
	s.Close()
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("the echo came back as %d bytes: %v", len(got), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen.Len() < 2*len(msg) {
		t.Fatalf("the relay passed on %d bytes; the message there and back is %d", seen.Len(), 2*len(msg))
	}
	if bytes.Contains(seen.Bytes(), []byte(marker)) {
		t.Fatal("the relay could read what it passed on")
	}
}

// What the relay may use follows its configuration, not the machine it runs on.
func TestResourceLimitsFollowTheConfiguration(t *testing.T) {
	cfg := Default()
	cfg.Limits.Servers, cfg.Limits.Circuits, cfg.Limits.MemoryMiB = 32, 8, 192
	l := resourceLimits(cfg).ToPartialLimitConfig()
	conns := 32*9 + 64
	if l.System.Memory != rcmgr.LimitVal64(192<<20) {
		t.Errorf("memory %v, want 192 MiB", l.System.Memory)
	}
	if l.System.ConnsInbound != rcmgr.LimitVal(conns) || l.System.Conns != rcmgr.LimitVal(conns) || l.System.FD != rcmgr.LimitVal(conns+64) {
		t.Errorf("connections %+v, want room for %d", l.System, conns)
	}
	if l.Transient.ConnsInbound >= l.System.ConnsInbound || l.Transient.Memory >= l.System.Memory {
		t.Errorf("connections not yet known may use as much as the rest: %+v", l.Transient)
	}
	// A bigger relay gets more, never less than its servers need.
	cfg.Limits.Servers, cfg.Limits.MemoryMiB = 1000, 2048
	l = resourceLimits(cfg).ToPartialLimitConfig()
	if int(l.System.ConnsInbound) < 1000*9 || l.System.Memory != rcmgr.LimitVal64(2048<<20) {
		t.Errorf("a relay for 1000 servers: %+v", l.System)
	}
	// One address holds its servers' share: four servers with eight circuits each, by default.
	if n := perAddress(Default()); n != 36 {
		t.Errorf("one address may hold %d connections, want 36", n)
	}
	small := Default()
	small.Limits.PerIP, small.Limits.Circuits = 1, 1
	if perAddress(small) != 8 {
		t.Error("fewer than libp2p's own eight per address")
	}
	cfg.Limits.MemoryMiB = 16
	if err := cfg.Check(); err == nil {
		t.Error("16 MiB was taken as enough")
	}
}

// Keys and the metrics token can come from files, so they needn't be in the
// environment that anyone inspecting the container sees.
func TestSecretsFromFiles(t *testing.T) {
	files := map[string]string{
		"/run/secrets/keys":   "first-key-0123456789abcd\r\nsecond-key-0123456789abc, third-key-0123456789abcde\n\n",
		"/run/secrets/metric": "  metrics-token-0123  \n",
	}
	was := readSecretFile
	t.Cleanup(func() { readSecretFile = was })
	readSecretFile = func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("no such file")
	}
	env := map[string]string{"WITTYRELAY_KEYS": "from-env-0123456789abcd", "WITTYRELAY_KEYS_FILE": "/run/secrets/keys", "WITTYRELAY_METRICS_TOKEN_FILE": "/run/secrets/metric"}
	c := Default()
	if err := c.fromEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Keys, "|") != "first-key-0123456789abcd|second-key-0123456789abc|third-key-0123456789abcde" {
		t.Errorf("keys from the file: %q", c.Keys)
	}
	if c.MetricsToken != "metrics-token-0123" {
		t.Errorf("token from the file: %q", c.MetricsToken)
	}
	if err := c.Check(); err != nil {
		t.Errorf("a good configuration from files: %v", err)
	}
	env["WITTYRELAY_KEYS_FILE"] = "/nowhere"
	c = Default()
	err := c.fromEnv(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "WITTYRELAY_KEYS_FILE") || strings.Contains(err.Error(), "from-env") {
		t.Errorf("an unreadable keys file: %v", err)
	}
}

// What a server calls itself is kept, logged and counted as plain text only.
func TestRegistrationNamesAreCleaned(t *testing.T) {
	r := startRelay(t, testConfig())
	a := node(t, 2)
	name := "Home\u202e\x1b[31mevil\x00" + strings.Repeat("é", 40)
	if res := register(t, a, r, RegisterRequest{V: 1, Key: testKey, Name: name}); !res.OK {
		t.Fatalf("%+v", res)
	}
	r.reg.mu.Lock()
	got := r.reg.servers[a.ID()].name
	r.reg.mu.Unlock()
	if len(got) > 60 || strings.ContainsAny(got, "\u202e\x1b\x00") || !strings.HasPrefix(got, "Home") {
		t.Fatalf("kept as %q", got)
	}
}

// A registration that is opened and then trickled, or never sent, is cut off.
func TestASlowRegistrationIsCutOff(t *testing.T) {
	cfg := testConfig()
	cfg.registerWait = 400 * time.Millisecond
	r := startRelay(t, cfg)
	h := node(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.Connect(ctx, relayInfo(r)); err != nil {
		t.Fatal(err)
	}
	for _, trickle := range []bool{false, true} {
		s, err := h.NewStream(ctx, r.Host().ID(), ProtoRegister)
		if err != nil {
			t.Fatal(err)
		}
		if trickle {
			_, _ = s.Write([]byte{0, 0, 1, 0})
			go func() {
				for range 50 {
					if _, err := s.Write([]byte{' '}); err != nil {
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
			}()
		}
		start := time.Now()
		_ = s.SetReadDeadline(time.Now().Add(15 * time.Second))
		_, _ = io.Copy(io.Discard, s)
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("trickle=%v: held for %v", trickle, d)
		}
		s.Reset()
	}
}

// ---------------------------------------------------------------- fuzzing

func FuzzRegister(f *testing.F) {
	ok, _ := json.Marshal(RegisterRequest{V: 1, Key: testKey, Name: "Home", Devices: []string{"12D3KooWD3eckifWpRn9wQpMG9R9hX3sD158z7EqHWmweQAJU5SA"}})
	f.Add(ok)
	f.Add([]byte(`{"v":1,"key":"","devices":null}`))
	f.Add([]byte(`{"v":1,"key":"` + testKey + `","devices":["QmYyQSo1c1Ym7orWxLYvCrM2EmxFTANf8wXmmE7DWjhx5N","x"]}`))
	f.Add([]byte(`{"v":2}`))
	cfg := testConfig()
	cfg.Limits.RegisterPerMinute = 1 << 30
	cfg.Limits.DevicesPerServer = 4
	server := mustID(f, 2)
	f.Fuzz(func(t *testing.T, b []byte) {
		var req RegisterRequest
		if json.Unmarshal(b, &req) != nil {
			return
		}
		reg := newRegistry(&cfg, newMetrics())
		res := reg.register(server, ipOf(ma.StringCast("/ip4/203.0.113.5/tcp/1")), req)
		if res.OK != (req.V == 1 && req.Key == testKey && len(req.Devices) <= 4) {
			t.Fatalf("answered %+v to %+v", res, req)
		}
		if !res.OK {
			if reg.count() != 0 {
				t.Fatal("a refused registration was kept")
			}
			return
		}
		s := reg.servers[server]
		if len(s.name) > 60 || len(s.devices) > len(req.Devices) {
			t.Fatalf("kept %+v", s)
		}
		for d := range s.devices {
			pub, err := d.ExtractPublicKey()
			if err != nil || pub == nil {
				t.Fatalf("kept a device ID with no key in it: %s", d)
			}
		}
	})
}

func FuzzConfig(f *testing.F) {
	f.Add(`{"access":"open","limits":{"servers":2,"circuits":1,"perIP":1,"registerPerMinute":1,"devicesPerServer":1,"memoryMiB":64}}`, "8000", "15m")
	f.Add(`{"announce":["/ip4/1.2.3.4/tcp/1"],"registerTTL":30}`, "x", "-1h")
	f.Add(`{`, "", "")
	f.Fuzz(func(t *testing.T, file, kbps, ttl string) {
		c := Default()
		c.Keys = []string{"a-test-access-key-0123456789"}
		dec := json.NewDecoder(strings.NewReader(file))
		dec.DisallowUnknownFields()
		if dec.Decode(&c) != nil {
			return
		}
		env := map[string]string{"WITTYRELAY_KBPS": kbps, "WITTYRELAY_REGISTER_TTL": ttl}
		if c.fromEnv(func(k string) string { return env[k] }) != nil {
			return
		}
		if c.Check() != nil {
			return
		}
		// What passes the check can be started from: limits sane, budgets positive.
		if c.Limits.Servers < 1 || c.Limits.Circuits < 1 || c.Limits.MemoryMiB < 64 || connBudget(c) <= 0 {
			t.Fatalf("passed the check: %+v", c.Limits)
		}
		_ = resourceLimits(c)
		_ = OfferOf(c)
	})
}

func mustID(f *testing.F, b byte) peer.ID {
	k, err := crypto.UnmarshalEd25519PrivateKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, 32)))
	if err != nil {
		f.Fatal(err)
	}
	id, err := peer.IDFromPrivateKey(k)
	if err != nil {
		f.Fatal(err)
	}
	return id
}
