// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	ma "github.com/multiformats/go-multiaddr"
)

// Config is everything the relay is set up with. It is read from a JSON file
// (WITTYRELAY_CONFIG, or /config/wittyrelay.json if it exists) and then from
// environment variables, which win.
type Config struct {
	// Name and Note are shown to server owners before they use the relay.
	Name string `json:"name"`
	Note string `json:"note"`
	// Port is listened on for QUIC (UDP) and TCP, IPv4 and IPv6.
	Port int `json:"port"`
	// Listen replaces the addresses listened on.
	Listen []string `json:"listen"`
	// Announce are the addresses servers are told to reach this relay at:
	// its public name or address. Without them, what libp2p finds is used.
	Announce []string `json:"announce"`
	// HTTP is where health and metrics are served ("" for nowhere).
	HTTP string `json:"http"`
	// DataDir keeps the relay's identity key.
	DataDir string `json:"dataDir"`
	// Access is "open" (any server may register) or "key" (only with one of Keys).
	Access string   `json:"access"`
	Keys   []string `json:"keys"`
	// AllowPrivate lets servers on private networks hold a place: for a relay
	// on a home or office network, or tests.
	AllowPrivate bool `json:"allowPrivate"`

	Limits Limits `json:"limits"`
	// RegisterTTL is how long a registration lasts unless renewed.
	RegisterTTL Duration `json:"registerTTL"`
	// MetricsToken, when set, must be sent as a bearer token to read /metrics.
	MetricsToken string `json:"metricsToken"`
	// LogLevel: debug | info | warn | error.
	LogLevel string `json:"logLevel"`

	// For tests: how long a registration may take (registerWait otherwise),
	// and what is shown every piece passed on (what an operator could see
	// on the wire, to prove it is only ciphertext).
	registerWait time.Duration
	tap          func([]byte)
}

// Limits are what the relay allows. Every one is told to servers before use.
type Limits struct {
	Kbps      int   `json:"kbps"`      // per connection, each way (0: none)
	TotalKbps int   `json:"totalKbps"` // everything together, each way (0: none)
	MaxHeight int   `json:"maxHeight"` // picture height streams should keep to (0: any)
	DataMiB   int64 `json:"dataMiB"`   // per connection, each way
	Minutes   int   `json:"minutes"`   // per connection
	Servers   int   `json:"servers"`   // registered at once
	Circuits  int   `json:"circuits"`  // connections per server
	PerIP     int   `json:"perIP"`     // servers from one IP address
	// RegisterPerMinute is how many registrations one IP address may make a minute.
	RegisterPerMinute int `json:"registerPerMinute"`
	// DevicesPerServer is the most devices one registration may list.
	DevicesPerServer int `json:"devicesPerServer"`
	// MemoryMiB is what the relay may use for connections and streams: set it
	// a little under the container's memory limit. libp2p would otherwise size
	// itself from the whole machine's memory, which a container doesn't have.
	MemoryMiB int `json:"memoryMiB"`
}

// Duration reads "15m" as well as a number of seconds.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		*d = Duration(v)
		return err
	}
	var n float64
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("a duration is \"15m\" or a number of seconds")
	}
	*d = Duration(time.Duration(n * float64(time.Second)))
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Default is a relay for a few friends' servers on a home connection.
func Default() Config {
	return Config{
		Name:    "WittyRelay",
		Port:    4001,
		HTTP:    ":8080",
		DataDir: "/data",
		Access:  "key",
		Limits: Limits{Kbps: 8000, TotalKbps: 50000, MaxHeight: 1080, DataMiB: 4096, Minutes: 180,
			Servers: 32, Circuits: 8, PerIP: 4, RegisterPerMinute: 10, DevicesPerServer: 512, MemoryMiB: 192},
		RegisterTTL: Duration(15 * time.Minute),
		LogLevel:    "info",
	}
}

// Load reads the file (if any) and the environment.
func Load() (Config, error) {
	c := Default()
	path := os.Getenv("WITTYRELAY_CONFIG")
	if path == "" {
		if _, err := os.Stat("/config/wittyrelay.json"); err == nil {
			path = "/config/wittyrelay.json"
		}
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("reading %s: %w", path, err)
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return c, fmt.Errorf("reading %s: %w", path, err)
		}
	}
	if err := c.fromEnv(os.Getenv); err != nil {
		return c, err
	}
	return c, c.Check()
}

func (c *Config) fromEnv(get func(string) string) error {
	str := func(name string, to *string) {
		if v, ok := lookup(get, name); ok {
			*to = v
		}
	}
	list := func(name string, to *[]string) {
		if v, ok := lookup(get, name); ok {
			*to = nil
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					*to = append(*to, p)
				}
			}
		}
	}
	var bad []string
	num := func(name string, to *int) {
		if v, ok := lookup(get, name); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				bad = append(bad, name)
				return
			}
			*to = n
		}
	}
	str("WITTYRELAY_NAME", &c.Name)
	str("WITTYRELAY_NOTE", &c.Note)
	num("WITTYRELAY_PORT", &c.Port)
	list("WITTYRELAY_LISTEN", &c.Listen)
	list("WITTYRELAY_ANNOUNCE", &c.Announce)
	str("WITTYRELAY_HTTP", &c.HTTP)
	str("WITTYRELAY_DATA_DIR", &c.DataDir)
	str("WITTYRELAY_ACCESS", &c.Access)
	list("WITTYRELAY_KEYS", &c.Keys)
	str("WITTYRELAY_METRICS_TOKEN", &c.MetricsToken)
	// Secrets can come from files instead (Docker secrets, a mounted file):
	// what is in the environment shows to anyone who can inspect the container.
	var unreadable []string
	secret := func(name string, apply func(string)) {
		if p, ok := lookup(get, name); ok {
			b, err := readSecretFile(p)
			if err != nil {
				unreadable = append(unreadable, name)
				return
			}
			apply(strings.TrimSpace(string(b)))
		}
	}
	secret("WITTYRELAY_KEYS_FILE", func(v string) {
		c.Keys = nil
		for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
			if p = strings.TrimSpace(p); p != "" {
				c.Keys = append(c.Keys, p)
			}
		}
	})
	secret("WITTYRELAY_METRICS_TOKEN_FILE", func(v string) { c.MetricsToken = v })
	str("WITTYRELAY_LOG_LEVEL", &c.LogLevel)
	if v, ok := lookup(get, "WITTYRELAY_ALLOW_PRIVATE"); ok {
		c.AllowPrivate = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	if v, ok := lookup(get, "WITTYRELAY_REGISTER_TTL"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			bad = append(bad, "WITTYRELAY_REGISTER_TTL")
		}
		c.RegisterTTL = Duration(d)
	}
	l := &c.Limits
	num("WITTYRELAY_KBPS", &l.Kbps)
	num("WITTYRELAY_TOTAL_KBPS", &l.TotalKbps)
	num("WITTYRELAY_MAX_HEIGHT", &l.MaxHeight)
	var mib int
	mib = int(l.DataMiB)
	num("WITTYRELAY_DATA_MIB", &mib)
	l.DataMiB = int64(mib)
	num("WITTYRELAY_MINUTES", &l.Minutes)
	num("WITTYRELAY_SERVERS", &l.Servers)
	num("WITTYRELAY_CIRCUITS", &l.Circuits)
	num("WITTYRELAY_PER_IP", &l.PerIP)
	num("WITTYRELAY_REGISTER_PER_MINUTE", &l.RegisterPerMinute)
	num("WITTYRELAY_DEVICES_PER_SERVER", &l.DevicesPerServer)
	num("WITTYRELAY_MEMORY_MIB", &l.MemoryMiB)
	if len(unreadable) > 0 {
		return fmt.Errorf("couldn't read the file named by %s", strings.Join(unreadable, ", "))
	}
	if len(bad) > 0 {
		return fmt.Errorf("not a number: %s", strings.Join(bad, ", "))
	}
	return nil
}

// readSecretFile reads a file named by a *_FILE variable. A variable for tests.
var readSecretFile = os.ReadFile

func lookup(get func(string) string, name string) (string, bool) {
	v := strings.TrimSpace(get(name))
	return v, v != ""
}

// Check refuses a configuration that would run badly or unsafely.
func (c *Config) Check() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	if c.Port < 1 || c.Port > 65535 {
		add("port %d is not a port", c.Port)
	}
	switch c.Access {
	case "open":
	case "key":
		if len(c.Keys) == 0 {
			add("access is \"key\" but no keys are set (WITTYRELAY_KEYS); make one with: wittyrelay key")
		}
		for _, k := range c.Keys {
			if len(k) < 16 {
				add("an access key is shorter than 16 characters: make one with: wittyrelay key")
				break
			}
			if strings.HasPrefix(strings.ToLower(k), "change-me") {
				add("an access key is still the example's placeholder: make one with: wittyrelay key")
				break
			}
		}
	default:
		add("access must be \"open\" or \"key\", not %q", c.Access)
	}
	for _, a := range append(append([]string{}, c.Listen...), c.Announce...) {
		if _, err := ma.NewMultiaddr(a); err != nil {
			add("%q is not a multiaddr", a)
		}
	}
	l := c.Limits
	if l.Kbps < 0 || l.TotalKbps < 0 || l.MaxHeight < 0 || l.DataMiB < 0 || l.Minutes < 0 {
		add("limits can't be negative")
	}
	if l.Kbps > 0 && l.Kbps < 100 {
		add("kbps under 100 is too slow to be any use")
	}
	if l.MaxHeight != 0 && (l.MaxHeight < 144 || l.MaxHeight > 4320) {
		add("maxHeight should be 0 (any) or between 144 and 4320")
	}
	if l.Servers < 1 || l.Servers > 100000 {
		add("servers should be between 1 and 100000")
	}
	if l.Circuits < 1 || l.Circuits > 1024 {
		add("circuits should be between 1 and 1024")
	}
	if l.PerIP < 1 {
		add("perIP should be at least 1")
	}
	if l.RegisterPerMinute < 1 {
		add("registerPerMinute should be at least 1")
	}
	if l.DevicesPerServer < 1 || l.DevicesPerServer > 1024 {
		add("devicesPerServer should be between 1 and 1024")
	}
	if l.MemoryMiB < 64 || l.MemoryMiB > 1<<20 {
		add("memoryMiB should be at least 64")
	}
	if time.Duration(c.RegisterTTL) < time.Minute || time.Duration(c.RegisterTTL) > 24*time.Hour {
		add("registerTTL should be between 1m and 24h")
	}
	if len(c.Name) > 60 || len(c.Note) > 280 {
		add("name is at most 60 characters and note at most 280")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
