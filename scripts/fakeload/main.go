// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

// Command fakeload puts made-up servers and devices on a relay you run, so
// its admin page has something to show: for screenshots and for trying a
// relay out. Each made-up server registers and holds a place; each of its
// devices connects through the relay and streams bytes there and back.
// Point it only at your own relay.
//
//	go run ./scripts/fakeload -relay /ip4/127.0.0.1/tcp/4001/p2p/12D3KooW... -key KEY -servers 4 -devices 2
//
// The relay needs WITTYRELAY_ALLOW_PRIVATE=1 (everything here comes from one
// private address) and a WITTYRELAY_PER_IP of at least -servers. libp2p takes
// about 8 connections from one address, so keep servers × (devices + 1) under it.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/wittywilla/wittyrelay/internal/relay"
)

const stream = "/fakeload/echo/1"

func main() {
	addr := flag.String("relay", "", "the relay's address, ending in /p2p/<its ID>")
	key := flag.String("key", "", "an access key, if the relay needs one")
	servers := flag.Int("servers", 4, "made-up servers")
	devices := flag.Int("devices", 2, "devices per server")
	kbps := flag.Int("kbps", 2000, "what each device sends, kbit/s (and gets back)")
	refusals := flag.Bool("refusals", true, "also try a wrong key and an unlisted device, so some refusals show")
	flag.Parse()
	info, err := peer.AddrInfoFromString(*addr)
	if err != nil {
		log.Fatalf("-relay: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	for i := range *servers {
		srv := newHost()
		var devs []host.Host
		var ids []string
		for range *devices {
			d := newHost()
			devs = append(devs, d)
			ids = append(ids, d.ID().String())
		}
		if err := register(ctx, srv, *info, *key, fmt.Sprintf("fake server %d", i+1), ids); err != nil {
			log.Printf("server %d: %v", i+1, err)
			continue
		}
		srv.SetStreamHandler(stream, func(s network.Stream) {
			defer s.Close()
			_, _ = io.Copy(s, s)
		})
		if _, err := client.Reserve(ctx, srv, *info); err != nil {
			log.Printf("server %d holding a place: %v", i+1, err)
			continue
		}
		go renew(ctx, srv, *info, *key, ids)
		for _, d := range devs {
			go device(ctx, d, *info, srv.ID(), *kbps)
		}
		log.Printf("server %d: registered with %d devices", i+1, len(devs))
	}
	if *refusals {
		h := newHost()
		_ = register(ctx, h, *info, "not-a-key-this-relay-knows", "wrong key", nil)
		stranger := newHost()
		go device(ctx, stranger, *info, peer.ID(""), 0)
	}
	<-ctx.Done()
}

func newHost() host.Host {
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"), libp2p.EnableRelay(), libp2p.DisableMetrics())
	if err != nil {
		log.Fatal(err)
	}
	return h
}

func register(ctx context.Context, h host.Host, r peer.AddrInfo, key, name string, devices []string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := h.Connect(ctx, r); err != nil {
		return err
	}
	s, err := h.NewStream(ctx, r.ID, relay.ProtoRegister)
	if err != nil {
		return err
	}
	defer s.Close()
	if devices == nil {
		devices = []string{}
	}
	b, _ := json.Marshal(relay.RegisterRequest{V: 1, Key: key, Name: name, Devices: devices})
	head := make([]byte, 4)
	binary.BigEndian.PutUint32(head, uint32(len(b)))
	if _, err := s.Write(append(head, b...)); err != nil {
		return err
	}
	if _, err := io.ReadFull(s, head); err != nil {
		return err
	}
	ans := make([]byte, binary.BigEndian.Uint32(head))
	if _, err := io.ReadFull(s, ans); err != nil {
		return err
	}
	var res relay.RegisterResponse
	if err := json.Unmarshal(ans, &res); err != nil {
		return err
	}
	if !res.OK {
		return errors.New(res.Error)
	}
	return nil
}

func renew(ctx context.Context, h host.Host, r peer.AddrInfo, key string, devices []string) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := register(ctx, h, r, key, "fake server", devices); err != nil {
				log.Printf("renewing: %v", err)
			}
		}
	}
}

// device connects to server through the relay and streams there and back,
// reconnecting when the relay ends the connection. With no server it asks
// for one that never listed it, once, to be refused.
func device(ctx context.Context, d host.Host, r peer.AddrInfo, server peer.ID, kbps int) {
	if server == "" {
		server = newHost().ID()
	}
	circuit := ma.StringCast(r.Addrs[0].String() + "/p2p/" + r.ID.String() + "/p2p-circuit")
	d.Peerstore().AddAddrs(r.ID, r.Addrs, peerstore.PermanentAddrTTL)
	for ctx.Err() == nil {
		d.Peerstore().AddAddrs(server, []ma.Multiaddr{circuit}, peerstore.TempAddrTTL)
		cctx, cancel := context.WithTimeout(network.WithAllowLimitedConn(ctx, "fakeload"), 30*time.Second)
		err := d.Connect(cctx, peer.AddrInfo{ID: server, Addrs: []ma.Multiaddr{circuit}})
		var s network.Stream
		if err == nil {
			s, err = d.NewStream(cctx, server, stream)
		}
		cancel()
		if err != nil {
			if kbps == 0 {
				return
			}
			time.Sleep(5 * time.Second)
			continue
		}
		go func() { _, _ = io.Copy(io.Discard, s) }()
		buf := make([]byte, 8<<10)
		every := time.Duration(float64(len(buf)*8) / float64(kbps*1000) * float64(time.Second))
		for ctx.Err() == nil {
			if _, err := s.Write(buf); err != nil {
				break
			}
			time.Sleep(every)
		}
		s.Close()
		_ = d.Network().ClosePeer(server)
		time.Sleep(2 * time.Second)
	}
}
