// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Settings are what the admin page may change. Access, keys, addresses and
// tokens are not among them: those stay in the file or the environment.
type Settings struct {
	Name     string `json:"name"`
	Note     string `json:"note"`
	LogLevel string `json:"logLevel"`

	Kbps              int   `json:"kbps"`
	TotalKbps         int   `json:"totalKbps"`
	MaxHeight         int   `json:"maxHeight"`
	DataMiB           int64 `json:"dataMiB"`
	Minutes           int   `json:"minutes"`
	Servers           int   `json:"servers"`
	Circuits          int   `json:"circuits"`
	PerIP             int   `json:"perIP"`
	RegisterPerMinute int   `json:"registerPerMinute"`
	DevicesPerServer  int   `json:"devicesPerServer"`
}

// SettingsOf is the part of c the admin page shows.
func SettingsOf(c Config) Settings {
	l := c.Limits
	return Settings{Name: c.Name, Note: c.Note, LogLevel: strings.ToLower(c.LogLevel),
		Kbps: l.Kbps, TotalKbps: l.TotalKbps, MaxHeight: l.MaxHeight, DataMiB: l.DataMiB, Minutes: l.Minutes,
		Servers: l.Servers, Circuits: l.Circuits, PerIP: l.PerIP, RegisterPerMinute: l.RegisterPerMinute,
		DevicesPerServer: l.DevicesPerServer}
}

func (s Settings) applyTo(c *Config) {
	c.Name, c.Note, c.LogLevel = s.Name, s.Note, s.LogLevel
	l := &c.Limits
	l.Kbps, l.TotalKbps, l.MaxHeight, l.DataMiB, l.Minutes = s.Kbps, s.TotalKbps, s.MaxHeight, s.DataMiB, s.Minutes
	l.Servers, l.Circuits, l.PerIP, l.RegisterPerMinute, l.DevicesPerServer = s.Servers, s.Circuits, s.PerIP, s.RegisterPerMinute, s.DevicesPerServer
}

// settingEnv is the environment variable that sets each setting. One that is
// set wins over the file, so the admin page shows it and can't change it.
var settingEnv = map[string]string{
	"name": "WITTYRELAY_NAME", "note": "WITTYRELAY_NOTE", "logLevel": "WITTYRELAY_LOG_LEVEL",
	"kbps": "WITTYRELAY_KBPS", "totalKbps": "WITTYRELAY_TOTAL_KBPS", "maxHeight": "WITTYRELAY_MAX_HEIGHT",
	"dataMiB": "WITTYRELAY_DATA_MIB", "minutes": "WITTYRELAY_MINUTES", "servers": "WITTYRELAY_SERVERS",
	"circuits": "WITTYRELAY_CIRCUITS", "perIP": "WITTYRELAY_PER_IP",
	"registerPerMinute": "WITTYRELAY_REGISTER_PER_MINUTE", "devicesPerServer": "WITTYRELAY_DEVICES_PER_SERVER",
}

// SettingsFromEnv says which settings the environment sets: setting -> variable.
func SettingsFromEnv(get func(string) string) map[string]string {
	out := map[string]string{}
	for k, env := range settingEnv {
		if _, ok := lookup(get, env); ok {
			out[k] = env
		}
	}
	return out
}

// ErrReadOnly is a configuration file that can't be written.
var ErrReadOnly = errors.New("the configuration file can't be written")

// SaveSettings checks s as the relay would run with it (the file, then the
// environment) and writes it to the file at path, keeping everything else in
// the file as it was. It returns the configuration the relay will have once
// restarted.
// Settings the environment sets (SettingsFromEnv) are left in the file as
// they were.
func SaveSettings(path string, s Settings, get func(string) string) (Config, error) {
	locked := SettingsFromEnv(get)
	raw := map[string]json.RawMessage{}
	c := Default()
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &raw); err != nil {
			return c, fmt.Errorf("reading %s: %w", path, err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return c, fmt.Errorf("reading %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return c, err
	}
	s.LogLevel = strings.ToLower(strings.TrimSpace(s.LogLevel))
	s.Name, s.Note = strings.TrimSpace(s.Name), strings.TrimSpace(s.Note)
	s.applyTo(&c)
	// What will run: the environment still wins.
	run := c
	run.Keys = append([]string(nil), c.Keys...)
	if err := run.fromEnv(get); err != nil {
		return run, err
	}
	if err := run.Check(); err != nil {
		return run, err
	}

	set := func(m map[string]json.RawMessage, k string, v any) {
		if _, env := locked[k]; env {
			return
		}
		b, _ := json.Marshal(v)
		m[k] = b
	}
	set(raw, "name", s.Name)
	set(raw, "note", s.Note)
	set(raw, "logLevel", s.LogLevel)
	limits := map[string]json.RawMessage{}
	if lb, ok := raw["limits"]; ok {
		_ = json.Unmarshal(lb, &limits)
	}
	for k, v := range map[string]any{"kbps": s.Kbps, "totalKbps": s.TotalKbps, "maxHeight": s.MaxHeight,
		"dataMiB": s.DataMiB, "minutes": s.Minutes, "servers": s.Servers, "circuits": s.Circuits, "perIP": s.PerIP,
		"registerPerMinute": s.RegisterPerMinute, "devicesPerServer": s.DevicesPerServer} {
		set(limits, k, v)
	}
	set(raw, "limits", limits)
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return run, err
	}
	if err := writeAtomic(path, append(out, '\n')); err != nil {
		return run, err
	}
	return run, nil
}

// writeAtomic writes a file next to path and renames it over path, so a
// crash leaves the old file or the new one, never half of one. The file may
// hold access keys: only its owner reads it.
func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: %v", ErrReadOnly, err)
	}
	f, err := os.CreateTemp(dir, ".wittyrelay-*.json")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrReadOnly, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("%w: %v", ErrReadOnly, err)
	}
	return nil
}
