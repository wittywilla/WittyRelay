// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package relay

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"github.com/libp2p/go-libp2p/core/protocol"
)

// The wire format, shared with WittyMediaServer (internal/p2p/relayinfo.go
// and wittyrelay.go there). Each message is a 4-byte big-endian length and
// that many bytes of JSON; one request and one answer per stream.

const (
	// ProtoRelay marks a relay that carries whole connections.
	ProtoRelay protocol.ID = "/wittymedia/relay/1.0.0"
	// ProtoInfo is answered with the Offer.
	ProtoInfo protocol.ID = "/wittymedia/relay-info/1.0.0"
	// ProtoRegister is how a server registers.
	ProtoRegister protocol.ID = "/wittyrelay/register/1.0.0"
)

// Offer is what the relay allows, told to servers before they use it.
type Offer struct {
	V         int    `json:"v"`
	Software  string `json:"software,omitempty"`
	Name      string `json:"name,omitempty"`
	Note      string `json:"note,omitempty"`
	Access    string `json:"access"`
	Kbps      int    `json:"kbps,omitempty"`
	TotalKbps int    `json:"totalKbps,omitempty"`
	MaxHeight int    `json:"maxHeight,omitempty"`
	DataMiB   int64  `json:"dataMiB,omitempty"`
	Minutes   int    `json:"minutes,omitempty"`
	Servers   int    `json:"servers,omitempty"`
	Circuits  int    `json:"circuits,omitempty"`
	// Health is how the relay is doing now. Not in OfferOf: it changes.
	Health *Health `json:"health,omitempty"`
}

// RegisterRequest is what a server sends.
type RegisterRequest struct {
	V       int      `json:"v"`
	Key     string   `json:"key,omitempty"`
	Name    string   `json:"name,omitempty"`
	Devices []string `json:"devices"`
}

// RegisterResponse is the answer.
type RegisterResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"` // key | full | busy | bad | off
	Error  string `json:"error,omitempty"`
	TTLSec int    `json:"ttlSec,omitempty"`
	Offer  Offer  `json:"offer"`
}

const (
	maxRegister = 64 << 10 // a registration listing 1024 devices is about 54 kB
	maxAnswer   = 4 << 10
)

var errTooLong = errors.New("message too long")

func writeMessage(w io.Writer, v any, limit int) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > limit {
		return errTooLong
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// readMessage reads one message, refusing one longer than limit before
// reading any of it.
func readMessage(r io.Reader, v any, limit int) error {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n == 0 || int64(n) > int64(limit) {
		return errTooLong
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
