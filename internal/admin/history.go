// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package admin

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

const sampleEvery = 5 * time.Second

type sample struct {
	at       time.Time
	bytes    int64
	circuits int64
}

// history is the last few minutes of the relay's counters, sampled every
// few seconds: enough for the rate now, the peak, connections a minute, and
// a small graph. The peak is kept since the start.
type history struct {
	mu     sync.Mutex
	max    int
	list   []sample
	peak   float64 // bits a second
	peakAt time.Time
}

func newHistory(n int) *history { return &history{max: n} }

func (h *history) add(at time.Time, bytes, circuits int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.list); n > 0 {
		prev := h.list[n-1]
		if dt := at.Sub(prev.at).Seconds(); dt > 0 {
			if bps := float64(bytes-prev.bytes) * 8 / dt; bps > h.peak {
				h.peak, h.peakAt = bps, at
			}
		}
	}
	if len(h.list) >= h.max {
		copy(h.list, h.list[1:])
		h.list = h.list[:len(h.list)-1]
	}
	h.list = append(h.list, sample{at: at, bytes: bytes, circuits: circuits})
}

type rates struct {
	Now         float64 // bits a second, over the last sample
	Peak        float64
	PeakAt      time.Time
	PerMinute   int64 // device connections opened in the last minute
	Rates       []float64
	SampleEvery time.Duration
}

func (h *history) rates() rates {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := rates{Peak: h.peak, PeakAt: h.peakAt, SampleEvery: sampleEvery}
	n := len(h.list)
	for i := 1; i < n; i++ {
		a, b := h.list[i-1], h.list[i]
		bps := 0.0
		if dt := b.at.Sub(a.at).Seconds(); dt > 0 {
			bps = float64(b.bytes-a.bytes) * 8 / dt
		}
		out.Rates = append(out.Rates, bps)
	}
	if len(out.Rates) > 0 {
		out.Now = out.Rates[len(out.Rates)-1]
	}
	if n > 0 {
		last := h.list[n-1]
		base := h.list[0]
		for i := n - 1; i >= 0; i-- {
			if last.at.Sub(h.list[i].at) >= time.Minute {
				base = h.list[i]
				break
			}
		}
		out.PerMinute = last.circuits - base.circuits
	}
	return out
}

// sparkline is the rates as an SVG polyline's points in a w by h box, the
// newest at the right. Server-drawn, so the page needs no chart script.
func sparkline(rs []float64, w, h float64, slots int) string {
	if len(rs) == 0 {
		return ""
	}
	top := 0.0
	for _, r := range rs {
		top = max(top, r)
	}
	if top <= 0 {
		top = 1
	}
	step := w / float64(max(slots-1, 1))
	var b strings.Builder
	start := w - step*float64(len(rs)-1)
	for i, r := range rs {
		x := start + step*float64(i)
		y := h - 2 - (r/top)*(h-4)
		fmt.Fprintf(&b, "%.1f,%.1f ", x, y)
	}
	return strings.TrimSpace(b.String())
}
