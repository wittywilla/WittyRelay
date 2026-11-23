// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"context"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/proto"
)

// libp2p's relay copies each relayed connection between two streams: the one
// the device opened to the relay (the "hop" stream) and the one the relay
// opens to the server (the "stop" stream). It has limits on time and amount
// but none on speed. The relay is given a host that hands it those two
// streams with their writes paced, so every relayed connection is held to
// Kbps each way, and everything together to TotalKbps. Nothing is forked.

// bucket paces writes to a rate in bytes per second. At most a quarter of a
// second's worth is ever saved up, so a burst stays short.
type bucket struct {
	mu   sync.Mutex
	rate float64
	have float64
	last time.Time
}

func newBucket(kbps int) *bucket {
	if kbps <= 0 {
		return nil
	}
	r := float64(kbps) * 1000 / 8
	return &bucket{rate: r, have: r / 4}
}

// wait blocks until n bytes may be sent, or ctx ends.
func (b *bucket) wait(ctx context.Context, n int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	now := time.Now()
	if !b.last.IsZero() {
		b.have = min(b.have+now.Sub(b.last).Seconds()*b.rate, b.rate/4)
	}
	b.last = now
	b.have -= float64(n)
	short := -b.have
	b.mu.Unlock()
	if short <= 0 {
		return
	}
	t := time.NewTimer(time.Duration(short / b.rate * float64(time.Second)))
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// pacedStream paces what is written to it.
type pacedStream struct {
	network.Stream
	ctx         context.Context
	own, shared *bucket
	m           *metrics
	tap         func([]byte)
}

func (s *pacedStream) Write(b []byte) (int, error) {
	if s.tap != nil {
		s.tap(b)
	}
	done := 0
	for done < len(b) {
		n := min(len(b)-done, 16<<10)
		s.own.wait(s.ctx, n)
		s.shared.wait(s.ctx, n)
		w, err := s.Stream.Write(b[done : done+n])
		done += w
		s.m.bytes.Add(int64(w))
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

// pacingHost is the host the relay is given.
type pacingHost struct {
	host.Host
	ctx    context.Context
	kbps   int
	shared *bucket
	m      *metrics
	tap    func([]byte)
}

func (h *pacingHost) pace(s network.Stream) network.Stream {
	return &pacedStream{Stream: s, ctx: h.ctx, own: newBucket(h.kbps), shared: h.shared, m: h.m, tap: h.tap}
}

// SetStreamHandler paces the hop streams devices and servers open.
func (h *pacingHost) SetStreamHandler(pid protocol.ID, handler network.StreamHandler) {
	if pid != proto.ProtoIDv2Hop {
		h.Host.SetStreamHandler(pid, handler)
		return
	}
	h.Host.SetStreamHandler(pid, func(s network.Stream) { handler(h.pace(s)) })
}

// NewStream paces the stop streams the relay opens to servers.
func (h *pacingHost) NewStream(ctx context.Context, p peer.ID, pids ...protocol.ID) (network.Stream, error) {
	s, err := h.Host.NewStream(ctx, p, pids...)
	if err != nil {
		return nil, err
	}
	for _, pid := range pids {
		if pid == proto.ProtoIDv2Stop {
			return h.pace(s), nil
		}
	}
	return s, nil
}
