// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	pbv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/pb"
)

// metrics are counted with atomics and written in Prometheus' text format by
// hand: a scrape costs nothing, and the relay needs no metrics library.
type metrics struct {
	start        time.Time
	servers      atomic.Int64 // registered now
	reservations atomic.Int64 // places held now
	circuits     atomic.Int64 // relayed connections open now
	circuitsAll  atomic.Int64
	bytes        atomic.Int64
	registered   atomic.Int64 // registrations accepted, renewals included

	mu      sync.Mutex
	refusal map[string]int64
}

func newMetrics() *metrics { return &metrics{start: time.Now(), refusal: map[string]int64{}} }

func (m *metrics) refused(reason string) {
	m.mu.Lock()
	m.refusal[reason]++
	m.mu.Unlock()
}

func (m *metrics) write(w io.Writer) {
	g := func(name, help, kind string, v int64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %d\n", name, help, name, kind, name, v)
	}
	g("wittyrelay_up_seconds", "Seconds since the relay started.", "gauge", int64(time.Since(m.start)/time.Second))
	g("wittyrelay_servers", "Servers registered now.", "gauge", m.servers.Load())
	g("wittyrelay_reservations", "Places held on the relay now.", "gauge", m.reservations.Load())
	g("wittyrelay_circuits", "Relayed connections open now.", "gauge", m.circuits.Load())
	g("wittyrelay_circuits_total", "Relayed connections opened.", "counter", m.circuitsAll.Load())
	g("wittyrelay_relayed_bytes_total", "Bytes passed on, both ways.", "counter", m.bytes.Load())
	g("wittyrelay_registrations_total", "Registrations accepted, renewals included.", "counter", m.registered.Load())
	m.mu.Lock()
	reasons := make([]string, 0, len(m.refusal))
	for k := range m.refusal {
		reasons = append(reasons, k)
	}
	sort.Strings(reasons)
	fmt.Fprintf(w, "# HELP wittyrelay_refused_total Requests refused, by reason.\n# TYPE wittyrelay_refused_total counter\n")
	for _, k := range reasons {
		fmt.Fprintf(w, "wittyrelay_refused_total{reason=%q} %d\n", k, m.refusal[k])
	}
	m.mu.Unlock()
}

// tracer is libp2p's relay reporting to these counters.
type tracer struct{ m *metrics }

func (t tracer) RelayStatus(bool) {}
func (t tracer) ConnectionOpened() {
	t.m.circuits.Add(1)
	t.m.circuitsAll.Add(1)
}
func (t tracer) ConnectionClosed(time.Duration) { t.m.circuits.Add(-1) }
func (t tracer) ConnectionRequestHandled(st pbv2.Status) {
	if st != pbv2.Status_OK {
		t.m.refused("circuit-" + statusWord(st))
	}
}
func (t tracer) ReservationAllowed(renewal bool) {
	if !renewal {
		t.m.reservations.Add(1)
	}
}
func (t tracer) ReservationClosed(n int) { t.m.reservations.Add(-int64(n)) }
func (t tracer) ReservationRequestHandled(st pbv2.Status) {
	if st != pbv2.Status_OK {
		t.m.refused("reservation-" + statusWord(st))
	}
}
func (t tracer) BytesTransferred(int) {} // counted where the bytes are paced

func statusWord(st pbv2.Status) string {
	switch st {
	case pbv2.Status_RESERVATION_REFUSED:
		return "refused"
	case pbv2.Status_RESOURCE_LIMIT_EXCEEDED:
		return "limit"
	case pbv2.Status_PERMISSION_DENIED:
		return "denied"
	case pbv2.Status_CONNECTION_FAILED:
		return "failed"
	case pbv2.Status_NO_RESERVATION:
		return "no-reservation"
	case pbv2.Status_MALFORMED_MESSAGE:
		return "malformed"
	}
	return "other"
}
