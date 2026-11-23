// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The relay's side of the admin page: its snapshot, the log ring and saving
// settings.

func TestSnapshotHidesWhoRegistered(t *testing.T) {
	r := startRelay(t, testConfig())
	srv := node(t, 2)
	dev := node(t, 3)
	res := register(t, srv, r, RegisterRequest{V: 1, Key: testKey, Name: "Sam's server", Devices: []string{dev.ID().String()}})
	if !res.OK {
		t.Fatalf("registration refused: %+v", res)
	}
	s := r.Snapshot()
	if s.Servers != 1 || len(s.Registered) != 1 {
		t.Fatalf("servers %d, registered %v", s.Servers, s.Registered)
	}
	got := s.Registered[0]
	if !strings.HasPrefix(got.ID, "srv-") || len(got.ID) != len("srv-")+6 || got.Devices != 1 {
		t.Fatalf("registered: %+v", got)
	}
	all, _ := json.Marshal(s)
	for _, secret := range []string{srv.ID().String(), short(srv.ID().String()), "Sam's server", testKey, dev.ID().String()} {
		if strings.Contains(string(all), secret) {
			t.Fatalf("the snapshot shows %q", secret)
		}
	}
	if s.Version != Version || !s.Ready || s.Started.IsZero() || s.Offer.Servers != testConfig().Limits.Servers {
		t.Fatalf("snapshot: %+v", s)
	}
	if again := r.Snapshot(); again.Registered[0].ID != got.ID {
		t.Fatal("a short ID changed within one run")
	}
}

func TestLogRingKeepsOnlyMessageReasonAndError(t *testing.T) {
	ring := NewLogRing(3)
	log := slog.New(RingHandler(slog.DiscardHandler, ring))
	log.Info("not kept: info")
	log.With("from", "203.0.113.9").Warn("registration refused", "reason", "key", "key", "the-secret-key", "server", "12D3KooW")
	log.Error("broke", "err", "disk full")
	for i := range 3 {
		log.Warn("filler", "n", i)
	}
	got := ring.Recent()
	if len(got) != 3 || got[0].Message != "filler" {
		t.Fatalf("ring: %+v", got)
	}
	ring = NewLogRing(10)
	log = slog.New(RingHandler(slog.DiscardHandler, ring))
	log.With("from", "203.0.113.9").Warn("registration refused", "reason", "key", "key", "the-secret-key", "server", "12D3KooW")
	log.Error("broke", "err", "disk full")
	got = ring.Recent()
	if len(got) != 2 || got[0].Err != "disk full" || got[1].Reason != "key" {
		t.Fatalf("ring: %+v", got)
	}
	b, _ := json.Marshal(got)
	for _, s := range []string{"203.0.113.9", "the-secret-key", "12D3KooW"} {
		if strings.Contains(string(b), s) {
			t.Fatalf("the ring kept %q: %s", s, b)
		}
	}
}

func TestSaveSettingsKeepsTheRestOfTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wittyrelay.json")
	orig := `{"access": "key", "keys": ["a-key-of-sixteen-chars"], "announce": ["/dns4/relay.example.com/tcp/4001"], "limits": {"kbps": 8000, "circuits": 8}}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"WITTYRELAY_SERVERS": "40"}
	get := func(k string) string { return env[k] }
	s := SettingsOf(Default())
	s.Kbps, s.Servers, s.LogLevel, s.Name = 6000, 99, "WARN", "  Mine  "
	run, err := SaveSettings(path, s, get)
	if err != nil {
		t.Fatal(err)
	}
	if run.Limits.Kbps != 6000 || run.Limits.Servers != 40 || run.LogLevel != "warn" || run.Name != "Mine" {
		t.Fatalf("would run with %+v", run)
	}
	b, _ := os.ReadFile(path)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	limits := raw["limits"].(map[string]any)
	if raw["access"] != "key" || raw["keys"] == nil || raw["announce"] == nil || limits["kbps"] != 6000.0 {
		t.Fatalf("file now: %s", b)
	}
	if _, ok := limits["servers"]; ok {
		t.Fatalf("a setting the environment sets was written to the file: %s", b)
	}

	// A bad value is refused and leaves the file as it was.
	s.Kbps = 50
	if _, err := SaveSettings(path, s, get); err == nil || !strings.Contains(err.Error(), "kbps") {
		t.Fatalf("kbps 50 was saved: %v", err)
	}
	if b2, _ := os.ReadFile(path); string(b2) != string(b) {
		t.Fatal("a refused save changed the file")
	}
	if got := SettingsFromEnv(get); len(got) != 1 || got["servers"] != "WITTYRELAY_SERVERS" {
		t.Fatalf("from env: %v", got)
	}
}

func TestSaveSettingsMakesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "wittyrelay.json")
	env := map[string]string{"WITTYRELAY_ACCESS": "open"}
	s := SettingsOf(Default())
	s.Minutes = 60
	if _, err := SaveSettings(path, s, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITTYRELAY_CONFIG", path)
	t.Setenv("WITTYRELAY_ACCESS", "open")
	c, err := Load()
	if err != nil || c.Limits.Minutes != 60 {
		t.Fatalf("loaded %+v, %v", c.Limits, err)
	}
}

func TestConfigAdmin(t *testing.T) {
	c := testConfig()
	c.Admin.Listen = "127.0.0.1:8081"
	if err := c.Check(); err != nil {
		t.Fatal(err)
	}
	c.Admin.Listen = "8081"
	if c.Check() == nil {
		t.Fatal("a port without a host part was accepted")
	}
	c.HTTP = ":8080"
	c.Admin.Listen = ":8080"
	if c.Check() == nil {
		t.Fatal("the admin page on the metrics port was accepted")
	}
	c.Admin.Listen = ""
	c.LogLevel = "loud"
	if c.Check() == nil {
		t.Fatal("log level loud accepted")
	}
	get := func(k string) string {
		return map[string]string{"WITTYRELAY_ADMIN": ":8081", "WITTYRELAY_ADMIN_HOSTS": "relay.example.com, admin.example.com"}[k]
	}
	d := Default()
	if err := d.fromEnv(get); err != nil || d.Admin.Listen != ":8081" || len(d.Admin.Hosts) != 2 {
		t.Fatalf("admin from env: %+v %v", d.Admin, err)
	}
	if Default().Admin.Listen != "" {
		t.Fatal("the admin page is on by default")
	}
}
