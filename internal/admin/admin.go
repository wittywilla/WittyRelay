// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

// Package admin is WittyRelay's admin page: the relay's status and health,
// its settings, and how to connect a server, for whoever runs it.
//
// It is off unless a listen address is set, and meant for localhost (in
// Docker: published to 127.0.0.1 on the host). Every page but sign-in needs
// the admin password, kept as an argon2id hash. Pages are rendered here with
// html/template; the stylesheet, one small script and the icon are embedded
// in the binary, and the Content-Security-Policy allows nothing else.
//
// It never shows what the relay carries, access keys, the metrics token, or
// who uses the relay: counts, and short IDs that mean nothing outside this
// run. docs/ADMIN.md lists every endpoint.
package admin

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wittywilla/wittyrelay/internal/relay"
)

//go:embed web
var webFS embed.FS

// Source is the relay as the page sees it.
type Source interface {
	Snapshot() relay.Snapshot
}

// Options are what the page is given besides the relay.
type Options struct {
	Config     relay.Config        // what the relay runs with
	ConfigPath string              // where settings are saved
	Env        func(string) string // the environment (os.Getenv)
	DataDir    string              // where admin.json is kept
	Ring       *relay.LogRing      // recent warnings and errors
	Level      *slog.LevelVar      // the log level, changed live
	Log        *slog.Logger
	Now        func() time.Time
	// Load reads the configuration as the relay would at its next start
	// (relay.Load), to tell whether a restart is waiting.
	Load func() (relay.Config, error)
	// SetupToken is the one-time token that sets the first password. Made
	// here when empty.
	SetupToken string
}

// Server is the admin page.
type Server struct {
	src  Source
	o    Options
	tpl  map[string]*template.Template
	sess *sessions
	lim  *limiter
	hist *history

	mu    sync.Mutex
	setup string // the setup token while there is no password; "" after
}

const (
	cookieSession = "__Host-wittyrelay"
	cookiePre     = "__Host-wittyrelay-pre" // the sign-in form's CSRF token, before there is a session
	maxForm       = 16 << 10
)

// New makes the admin page for src.
func New(src Source, o Options) (*Server, error) {
	if o.Env == nil {
		o.Env = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Ring == nil {
		o.Ring = relay.NewLogRing(50)
	}
	if o.Level == nil {
		o.Level = new(slog.LevelVar)
	}
	if o.Load == nil {
		o.Load = relay.Load
	}
	tpl, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	s := &Server{src: src, o: o, tpl: tpl, sess: newSessions(o.Now),
		lim: newLimiter(5, 30, 15*time.Minute, o.Now), hist: newHistory(120)}
	if !s.hasPassword() {
		s.setup = o.SetupToken
		if s.setup == "" {
			s.setup = randomToken(18)
		}
	}
	s.Sample()
	return s, nil
}

// SetupToken is the token that sets the first password, or "" once one is set.
func (s *Server) SetupToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setup != "" && s.hasPassword() {
		s.setup = "" // set from the command line meanwhile
	}
	return s.setup
}

func (s *Server) hasPassword() bool {
	_, err := readHash(s.o.DataDir)
	return err == nil
}

// Run samples the relay's counters every few seconds until ctx ends, for
// rates, peaks and the bandwidth graph.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sample()
		}
	}
}

// Sample reads the counters once, as Run does every few seconds.
func (s *Server) Sample() {
	snap := s.src.Snapshot()
	s.hist.add(s.o.Now(), snap.BytesTotal, snap.CircuitsTotal)
}

// HTTPServer is Handler with timeouts.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8 << 10}
}

// Handler serves every endpoint (docs/ADMIN.md).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web/static")
	files := http.FileServerFS(static)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", noListing(files)))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/assets/icon.svg", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /{$}", s.root)
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup", s.setupPost)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginPost)
	mux.HandleFunc("POST /logout", s.signedIn(s.logout))
	mux.HandleFunc("GET /status", s.signedIn(s.statusPage))
	mux.HandleFunc("GET /health", s.signedIn(s.healthPage))
	mux.HandleFunc("GET /connect", s.signedIn(s.connectPage))
	mux.HandleFunc("GET /settings", s.signedIn(s.settingsPage))
	mux.HandleFunc("POST /settings", s.signedIn(s.settingsPost))
	mux.HandleFunc("POST /password", s.signedIn(s.passwordPost))
	mux.HandleFunc("GET /api/status", s.signedIn(s.apiStatus))
	return s.guard(mux)
}

// guard is around everything: security headers, the Host check, and a
// cross-site check and size limit on every POST.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; "+
			"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin") // not no-referrer: with it, browsers send "Origin: null" on a form post
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			h.Set("Cache-Control", "no-store")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		if !s.hostAllowed(r.Host) {
			http.Error(w, "This page isn't served by that name. Add it to WITTYRELAY_ADMIN_HOSTS.", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !sameOrigin(r) {
				http.Error(w, "Refused: this came from another site.", http.StatusForbidden)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxForm)
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowed refuses names that aren't this machine's, an address, or one
// set in Admin.Hosts, so a site that points its own name at 127.0.0.1 (DNS
// rebinding) is refused.
func (s *Server) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if net.ParseIP(host) != nil {
		return true
	}
	for _, h := range s.o.Config.Admin.Hosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// sameOrigin: a form posted from another site is refused, by the browser's
// own word (Sec-Fetch-Site) and by Origin, before the CSRF token is looked at.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}

// secureContext says whether the browser will keep a Secure cookie: over
// HTTPS (here or at a proxy in front), or from this machine.
func secureContext(r *http.Request) bool {
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func noListing(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

type ctxKey struct{}

type signed struct {
	token string
	sess  *session
}

// signedIn lets a request through only with a live session, and a POST only
// with that session's CSRF token as well.
func (s *Server) signedIn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var sess *session
		c, err := r.Cookie(cookieSession)
		if err == nil {
			sess = s.sess.get(c.Value)
		}
		if sess == nil {
			if strings.HasPrefix(r.URL.Path, "/api/") || r.Method != http.MethodGet {
				http.Error(w, "Sign in first.", http.StatusUnauthorized)
				return
			}
			to := "/login"
			if s.SetupToken() != "" {
				to = "/setup"
			}
			http.Redirect(w, r, to, http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "That form is too big.", http.StatusRequestEntityTooLarge)
				return
			}
			if !tokenEqual(r.PostForm.Get("csrf"), sess.csrf) {
				http.Error(w, "This form has expired. Go back, reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, signed{token: c.Value, sess: sess})))
	}
}

func current(r *http.Request) signed {
	v, _ := r.Context().Value(ctxKey{}).(signed)
	return v
}

func tokenEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	x, y := digest(a), digest(b)
	return x == y
}

func setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// preToken is the CSRF token of the sign-in and setup forms: a cookie and the
// same value in the form (double submit), since there is no session yet.
func preToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(cookiePre); err == nil && len(c.Value) >= 32 && len(c.Value) <= 64 {
		return c.Value
	}
	t := randomToken(32)
	setCookie(w, cookiePre, t, 3600)
	return t
}

func preTokenOK(r *http.Request) bool {
	c, err := r.Cookie(cookiePre)
	return err == nil && tokenEqual(c.Value, r.PostForm.Get("csrf"))
}

func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) startSession(w http.ResponseWriter) {
	token, _ := s.sess.start()
	setCookie(w, cookieSession, token, int(sessionMax/time.Second))
	setCookie(w, cookiePre, "", -1)
}
