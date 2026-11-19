// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

// Command wittyrelay runs a WittyRelay.
//
//	wittyrelay            run (configured by file and environment: see README)
//	wittyrelay key        print a new access key for a server
//	wittyrelay id         print the relay's ID (making its key if there is none)
//	wittyrelay health     exit 0 if the relay on this machine answers (for Docker)
//	wittyrelay version
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/wittywilla/wittyrelay/internal/relay"
)

func main() {
	if len(os.Args) > 1 {
		os.Exit(command(os.Args[1]))
	}
	os.Exit(run())
}

func command(name string) int {
	switch name {
	case "key":
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(base64.RawURLEncoding.EncodeToString(b))
		return 0
	case "id":
		cfg, err := relay.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		k, err := relay.LoadKey(cfg.DataDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		id, _ := peer.IDFromPrivateKey(k)
		fmt.Println(id.String())
		return 0
	case "health":
		addr := os.Getenv("WITTYRELAY_HTTP")
		if addr == "" {
			addr = ":8080"
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return 1
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		c := http.Client{Timeout: 3 * time.Second}
		res, err := c.Get("http://" + net.JoinHostPort(host, port) + "/readyz")
		if err != nil {
			return 1
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			return 1
		}
		return 0
	case "version", "-version", "--version":
		fmt.Println("wittyrelay", relay.Version)
		return 0
	case "help", "-h", "--help":
		fmt.Println("wittyrelay [key|id|health|version]: see README.md for the configuration")
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n", name)
	return 2
}

func run() int {
	cfg, err := relay.Load()
	lvl := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
	if err != nil {
		log.Error("the configuration isn't usable", "err", err)
		return 2
	}
	key, err := relay.LoadKey(cfg.DataDir)
	if err != nil {
		log.Error("the relay's key couldn't be read or made", "dir", cfg.DataDir, "err", err)
		return 1
	}
	r, err := relay.Start(cfg, key, log)
	if err != nil {
		log.Error("the relay couldn't start", "err", err)
		return 1
	}
	var srv *http.Server
	if cfg.HTTP != "" {
		srv = r.HTTPServer(cfg.HTTP)
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("health and metrics stopped", "err", err)
			}
		}()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("stopping")
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	}
	_ = r.Close()
	return 0
}
