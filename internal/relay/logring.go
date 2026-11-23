// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// LogEntry is a warning or error kept for the admin page.
type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Reason  string    `json:"reason,omitempty"`
	Err     string    `json:"err,omitempty"`
}

// LogRing keeps the last warnings and errors. Only the message and two
// attributes are kept (reason and err), never addresses, names or anything
// else a log line carries, so nothing secret or personal reaches the page.
type LogRing struct {
	mu   sync.Mutex
	max  int
	list []LogEntry
}

// NewLogRing keeps the last n entries.
func NewLogRing(n int) *LogRing { return &LogRing{max: n} }

// Add keeps e, dropping the oldest when full.
func (r *LogRing) Add(e LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.list) >= r.max {
		copy(r.list, r.list[1:])
		r.list = r.list[:len(r.list)-1]
	}
	r.list = append(r.list, e)
}

// Recent is newest first.
func (r *LogRing) Recent() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.list))
	for i, e := range r.list {
		out[len(r.list)-1-i] = e
	}
	return out
}

// RingHandler passes every record to next and keeps warnings and errors in ring.
func RingHandler(next slog.Handler, ring *LogRing) slog.Handler {
	return &ringHandler{next: next, ring: ring}
}

type ringHandler struct {
	next  slog.Handler
	ring  *LogRing
	attrs []slog.Attr
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn || h.next.Enabled(ctx, l)
}

func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelWarn {
		e := LogEntry{Time: rec.Time, Level: rec.Level.String(), Message: clip(rec.Message, 300)}
		take := func(a slog.Attr) bool {
			switch a.Key {
			case "reason":
				e.Reason = clip(a.Value.String(), 60)
			case "err":
				e.Err = clip(a.Value.String(), 300)
			}
			return true
		}
		for _, a := range h.attrs {
			take(a)
		}
		rec.Attrs(take)
		h.ring.Add(e)
	}
	if h.next.Enabled(ctx, rec.Level) {
		return h.next.Handle(ctx, rec)
	}
	return nil
}

func (h *ringHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &ringHandler{next: h.next.WithAttrs(as), ring: h.ring, attrs: append(append([]slog.Attr{}, h.attrs...), as...)}
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	return &ringHandler{next: h.next.WithGroup(name), ring: h.ring, attrs: h.attrs}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
