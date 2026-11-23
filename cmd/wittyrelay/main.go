// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

// Command wittyrelay runs a WittyRelay.
//
//	wittyrelay            run (configured by file and environment: see README)
//	wittyrelay key        print a new access key for a server
//	wittyrelay id         print the relay's ID (making its key if there is none)
//	wittyrelay health     exit 0 if the relay on this machine answers (for Docker)
//	wittyrelay admin-password   set the admin page's password, read from standard input
//	wittyrelay version
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/wittywilla/wittyrelay/internal/admin"
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
	case "admin-password":
		return adminPassword()
	case "version", "-version", "--version":
		fmt.Println("wittyrelay", relay.Version)
		return 0
	case "help", "-h", "--help":
		fmt.Println("wittyrelay [key|id|health|admin-password|version]: see README.md for the configuration")
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n", name)
	return 2
}

// adminPassword sets the admin page's password from standard input:
//
//	docker compose exec -T wittyrelay wittyrelay admin-password <<< 'a new password'
func adminPassword() int {
	dir := strings.TrimSpace(os.Getenv("WITTYRELAY_DATA_DIR"))
	if cfg, err := relay.Load(); err == nil {
		dir = cfg.DataDir
	} else if dir == "" {
		dir = relay.Default().DataDir
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "New admin password (shown as you type; or pipe it in): ")
	}
	line, err := bufio.NewReader(io.LimitReader(os.Stdin, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	pw := strings.TrimRight(line, "\r\n")
	if msg := admin.CheckNewPassword(pw, pw); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		return 1
	}
	if err := admin.SetPassword(dir, pw); err != nil {
		fmt.Fprintln(os.Stderr, "the password couldn't be kept:", err)
		return 1
	}
	fmt.Println("The admin password is set. Sign in on the admin page.")
	return 0
}

func run() int {
	cfg, err := relay.Load()
	level := new(slog.LevelVar)
	level.Set(admin.LevelOf(cfg.LogLevel))
	ring := relay.NewLogRing(50)
	log := slog.New(relay.RingHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}), ring))
	if err != nil {
		log.Error("the configuration isn't usable", "err", err)
		return 2
	}
	// The garbage collector works to the same budget as the connections, plus
	// room for the program itself, unless GOMEMLIMIT says otherwise: so the
	// relay slows down to stay inside a container's limit instead of being killed.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(int64(cfg.Limits.MemoryMiB+48) << 20)
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
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var adm *http.Server
	if cfg.Admin.Listen != "" {
		path, _ := relay.ConfigPath()
		a, err := admin.New(r, admin.Options{Config: cfg, ConfigPath: path, DataDir: cfg.DataDir, Ring: ring, Level: level, Log: log})
		if err != nil {
			log.Error("the admin page couldn't start", "err", err)
			return 1
		}
		go a.Run(ctx)
		adm = a.HTTPServer(cfg.Admin.Listen)
		go func() {
			if err := adm.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("the admin page stopped", "err", err)
			}
		}()
		log.Info("admin page", "listen", cfg.Admin.Listen)
		if tok := a.SetupToken(); tok != "" {
			// Straight to the log, past the level and the page's own list of warnings.
			slog.New(slog.NewJSONHandler(os.Stdout, nil)).Info("admin page: no password yet. Open it and set one with this setup token",
				"setup token", tok, "or", "wittyrelay admin-password")
		}
	} else {
		log.Info("admin page off; set WITTYRELAY_ADMIN to turn it on")
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("stopping")
	stop()
	for _, s := range []*http.Server{srv, adm} {
		if s != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Shutdown(ctx)
			cancel()
		}
	}
	_ = r.Close()
	return 0
}
