// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wittywilla/wittyrelay/internal/admin/fake"
	"github.com/wittywilla/wittyrelay/internal/relay"
)

// Secrets the page must never show, set in the configuration the tests use.
const (
	secretKey     = "SECRET-ACCESS-KEY-0123456789"
	secretMetrics = "SECRET-METRICS-TOKEN-0123456789"
	setupToken    = "SECRET-SETUP-TOKEN-0123456789"
	password      = "a long test password"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	t     *testing.T
	s     *Server
	h     http.Handler
	dir   string
	path  string
	env   map[string]string
	clock *clock
	level *slog.LevelVar
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wittyrelay.json")
	cfg := relay.Default()
	cfg.Keys = []string{secretKey}
	cfg.MetricsToken = secretMetrics
	cfg.DataDir = dir
	cfg.Admin.Listen = "127.0.0.1:0"
	cfg.Admin.Hosts = []string{"relay-admin.example.com"}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	hz := &harness{t: t, dir: dir, path: path, env: map[string]string{}, clock: &clock{t: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)},
		level: new(slog.LevelVar)}
	get := func(k string) string { return hz.env[k] }
	load := func() (relay.Config, error) {
		c := relay.Default()
		b, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return c, err
		}
		return c, c.Check()
	}
	src := fake.New(cfg)
	src.Now = hz.clock.now
	ring := relay.NewLogRing(10)
	log := slog.New(relay.RingHandler(slog.DiscardHandler, ring))
	s, err := New(src, Options{Config: cfg, ConfigPath: path, Env: get, DataDir: dir, Ring: ring, Level: hz.level,
		Log: log, Now: hz.clock.now, Load: load, SetupToken: setupToken})
	if err != nil {
		t.Fatal(err)
	}
	hz.s, hz.h = s, s.Handler()
	return hz
}

type req struct {
	method, path string
	form         url.Values
	cookies      map[string]string
	header       map[string]string
}

func (hz *harness) do(r req) *http.Response {
	hz.t.Helper()
	var body io.Reader
	if r.form != nil {
		body = strings.NewReader(r.form.Encode())
	}
	hr := httptest.NewRequest(r.method, "http://127.0.0.1:8081"+r.path, body)
	hr.RemoteAddr = "127.0.0.1:50000"
	if r.form != nil {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range r.header {
		if k == "Host" {
			hr.Host = v
			continue
		}
		hr.Header.Set(k, v)
	}
	for k, v := range r.cookies {
		hr.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	hz.h.ServeHTTP(w, hr)
	return w.Result()
}

func cookie(res *http.Response, name string) string {
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func text(res *http.Response) string {
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

// pre gets the sign-in forms' CSRF cookie.
func (hz *harness) pre(path string) string {
	hz.t.Helper()
	res := hz.do(req{method: "GET", path: path})
	if res.StatusCode != 200 {
		hz.t.Fatalf("GET %s: %d", path, res.StatusCode)
	}
	v := cookie(res, cookiePre)
	if v == "" {
		hz.t.Fatalf("GET %s set no %s", path, cookiePre)
	}
	return v
}

// setup sets the password and returns the session cookie and its CSRF token.
func (hz *harness) setup() (string, string) {
	hz.t.Helper()
	pre := hz.pre("/setup")
	res := hz.do(req{method: "POST", path: "/setup", cookies: map[string]string{cookiePre: pre},
		form: url.Values{"csrf": {pre}, "token": {setupToken}, "password": {password}, "again": {password}}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/connect?welcome=1" {
		hz.t.Fatalf("setup: %d %s", res.StatusCode, text(res))
	}
	return hz.session(res)
}

func (hz *harness) session(res *http.Response) (string, string) {
	hz.t.Helper()
	tok := cookie(res, cookieSession)
	sess := hz.s.sess.get(tok)
	if sess == nil {
		hz.t.Fatal("no session")
	}
	return tok, sess.csrf
}

func (hz *harness) login(pw string) *http.Response {
	pre := hz.pre("/login")
	return hz.do(req{method: "POST", path: "/login", cookies: map[string]string{cookiePre: pre},
		form: url.Values{"csrf": {pre}, "password": {pw}}})
}

// Every page and the API need a session; every POST needs its CSRF token and
// a same-site origin as well.
func TestEveryEndpointNeedsAuth(t *testing.T) {
	hz := newHarness(t)
	tok, csrf := hz.setup()

	gets := []string{"/status", "/health", "/connect", "/settings", "/api/status"}
	for _, p := range gets {
		for _, c := range []map[string]string{nil, {cookieSession: "made-up"}, {cookiePre: "made-up-but-long-enough-to-count-as-a-token"}} {
			res := hz.do(req{method: "GET", path: p, cookies: c})
			if p == "/api/status" {
				if res.StatusCode != http.StatusUnauthorized {
					t.Errorf("GET %s with %v: %d, want 401", p, c, res.StatusCode)
				}
				continue
			}
			if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
				t.Errorf("GET %s with %v: %d %s, want 303 to /login", p, c, res.StatusCode, res.Header.Get("Location"))
			}
		}
		res := hz.do(req{method: "GET", path: p, cookies: map[string]string{cookieSession: tok}})
		if res.StatusCode != 200 {
			t.Errorf("GET %s signed in: %d", p, res.StatusCode)
		}
	}

	posts := map[string]url.Values{
		"/settings": {"kbps": {"7000"}},
		"/password": {"current": {password}, "password": {"another long password"}, "again": {"another long password"}},
		"/logout":   {},
	}
	for p, form := range posts {
		with := func(f url.Values, extra map[string]string) url.Values {
			out := url.Values{}
			for k, v := range f {
				out[k] = v
			}
			for k, v := range extra {
				out.Set(k, v)
			}
			return out
		}
		cases := []struct {
			name string
			r    req
			want int
		}{
			{"no session", req{form: with(form, map[string]string{"csrf": csrf})}, 401},
			{"made-up session", req{form: with(form, map[string]string{"csrf": csrf}), cookies: map[string]string{cookieSession: "x"}}, 401},
			{"no csrf", req{form: form, cookies: map[string]string{cookieSession: tok}}, 403},
			{"wrong csrf", req{form: with(form, map[string]string{"csrf": "nope"}), cookies: map[string]string{cookieSession: tok}}, 403},
			{"other origin", req{form: with(form, map[string]string{"csrf": csrf}), cookies: map[string]string{cookieSession: tok},
				header: map[string]string{"Origin": "https://evil.example"}}, 403},
			{"cross-site", req{form: with(form, map[string]string{"csrf": csrf}), cookies: map[string]string{cookieSession: tok},
				header: map[string]string{"Sec-Fetch-Site": "cross-site"}}, 403},
			{"null origin", req{form: with(form, map[string]string{"csrf": csrf}), cookies: map[string]string{cookieSession: tok},
				header: map[string]string{"Origin": "null"}}, 403},
		}
		for _, c := range cases {
			c.r.method, c.r.path = "POST", p
			if res := hz.do(c.r); res.StatusCode != c.want {
				t.Errorf("POST %s, %s: %d, want %d", p, c.name, res.StatusCode, c.want)
			}
		}
	}
	// And with everything right: settings last-but-one, then sign out.
	res := hz.do(req{method: "POST", path: "/settings", cookies: map[string]string{cookieSession: tok},
		header: map[string]string{"Origin": "http://127.0.0.1:8081", "Sec-Fetch-Site": "same-origin"},
		form:   url.Values{"csrf": {csrf}, "kbps": {"7000"}}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /settings: %d %s", res.StatusCode, text(res))
	}
	res = hz.do(req{method: "POST", path: "/logout", cookies: map[string]string{cookieSession: tok}, form: url.Values{"csrf": {csrf}}})
	if res.StatusCode != http.StatusSeeOther || hz.s.sess.get(tok) != nil {
		t.Fatalf("logout: %d, session still there: %v", res.StatusCode, hz.s.sess.get(tok) != nil)
	}
	if res := hz.do(req{method: "GET", path: "/status", cookies: map[string]string{cookieSession: tok}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("signed out, /status: %d", res.StatusCode)
	}
}

func TestPublicEndpoints(t *testing.T) {
	hz := newHarness(t)
	for _, p := range []string{"/assets/app.css", "/assets/app.js", "/assets/icon.svg", "/setup"} {
		if res := hz.do(req{method: "GET", path: p}); res.StatusCode != 200 {
			t.Errorf("GET %s: %d", p, res.StatusCode)
		}
	}
	for _, p := range []string{"/assets/", "/assets/nope.js", "/nope", "/layout.html", "/web/layout.html", "/assets/../layout.html"} {
		if res := hz.do(req{method: "GET", path: p}); res.StatusCode != 404 && res.StatusCode != 307 {
			t.Errorf("GET %s: %d", p, res.StatusCode)
		}
	}
	// Before a password: everything leads to setup, and login does too.
	for _, p := range []string{"/", "/login", "/status"} {
		res := hz.do(req{method: "GET", path: p})
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/setup" {
			t.Errorf("GET %s before setup: %d %s", p, res.StatusCode, res.Header.Get("Location"))
		}
	}
	if res := hz.do(req{method: "PUT", path: "/settings"}); res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT /settings: %d", res.StatusCode)
	}
}

func TestSetup(t *testing.T) {
	hz := newHarness(t)
	pre := hz.pre("/setup")
	post := func(f url.Values, c map[string]string) *http.Response {
		return hz.do(req{method: "POST", path: "/setup", form: f, cookies: c})
	}
	ok := map[string]string{cookiePre: pre}
	if res := post(url.Values{"csrf": {pre}, "token": {setupToken}, "password": {password}, "again": {password}}, nil); res.StatusCode != 403 {
		t.Fatalf("no CSRF cookie: %d", res.StatusCode)
	}
	if res := post(url.Values{"csrf": {"other"}, "token": {setupToken}, "password": {password}, "again": {password}}, ok); res.StatusCode != 403 {
		t.Fatalf("CSRF field not the cookie: %d", res.StatusCode)
	}
	if res := post(url.Values{"csrf": {pre}, "token": {"wrong"}, "password": {password}, "again": {password}}, ok); res.StatusCode != 401 {
		t.Fatalf("wrong token: %d", res.StatusCode)
	}
	if res := post(url.Values{"csrf": {pre}, "token": {setupToken}, "password": {"short"}, "again": {"short"}}, ok); res.StatusCode != 422 {
		t.Fatalf("short password: %d", res.StatusCode)
	}
	if res := post(url.Values{"csrf": {pre}, "token": {setupToken}, "password": {password}, "again": {password + "x"}}, ok); res.StatusCode != 422 {
		t.Fatalf("passwords differ: %d", res.StatusCode)
	}
	hz.setup()
	if hz.s.SetupToken() != "" {
		t.Fatal("the setup token still works after setup")
	}
	b, _ := os.ReadFile(PasswordPath(hz.dir))
	if !strings.Contains(string(b), "$argon2id$") || strings.Contains(string(b), password) {
		t.Fatalf("admin.json: %s", b)
	}
	// The token can't set the password again.
	res := post(url.Values{"csrf": {pre}, "token": {setupToken}, "password": {"an attacker's password"}, "again": {"an attacker's password"}}, ok)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
		t.Fatalf("setup again: %d", res.StatusCode)
	}
	if res := hz.do(req{method: "GET", path: "/setup"}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /setup after setup: %d", res.StatusCode)
	}
}

func TestLoginAndRateLimit(t *testing.T) {
	hz := newHarness(t)
	hz.setup()
	if res := hz.login(password); res.StatusCode != http.StatusSeeOther || cookie(res, cookieSession) == "" {
		t.Fatalf("right password: %d", res.StatusCode)
	}
	for i := range 5 {
		if res := hz.login("wrong password " + string(rune('a'+i))); res.StatusCode != 401 {
			t.Fatalf("wrong password %d: %d", i, res.StatusCode)
		}
	}
	res := hz.login(password)
	if res.StatusCode != http.StatusTooManyRequests || cookie(res, cookieSession) != "" {
		t.Fatalf("after 5 wrong tries, the right one: %d", res.StatusCode)
	}
	hz.clock.add(16 * time.Minute)
	if res := hz.login(password); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("after the wait: %d", res.StatusCode)
	}
	// The session cookie is Secure, HttpOnly, SameSite=Strict and __Host-.
	res = hz.login(password)
	for _, c := range res.Cookies() {
		if c.Name == cookieSession && (!c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "") {
			t.Fatalf("session cookie: %+v", c)
		}
	}
	// Without a CSRF cookie, no sign-in.
	r := hz.do(req{method: "POST", path: "/login", form: url.Values{"csrf": {"x"}, "password": {password}}})
	if r.StatusCode != 403 {
		t.Fatalf("login without CSRF: %d", r.StatusCode)
	}
}

func TestLoginOnlyWhereTheCookieWillBeKept(t *testing.T) {
	hz := newHarness(t)
	hz.setup()
	pre := hz.pre("/login")
	res := hz.do(req{method: "POST", path: "/login", cookies: map[string]string{cookiePre: pre},
		header: map[string]string{"Host": "192.168.1.20:8081"}, form: url.Values{"csrf": {pre}, "password": {password}}})
	if res.StatusCode != 403 {
		t.Fatalf("plain HTTP from the LAN: %d", res.StatusCode)
	}
	res = hz.do(req{method: "POST", path: "/login", cookies: map[string]string{cookiePre: pre},
		header: map[string]string{"Host": "relay-admin.example.com", "X-Forwarded-Proto": "https"}, form: url.Values{"csrf": {pre}, "password": {password}}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("behind an HTTPS proxy: %d", res.StatusCode)
	}
}

func TestHostCheck(t *testing.T) {
	hz := newHarness(t)
	for host, want := range map[string]int{"localhost:8081": 303, "127.0.0.1:8081": 303, "[::1]:8081": 303, "10.0.0.5:8081": 303,
		"relay-admin.example.com": 303, "evil.example:8081": 421, "localhost.evil.example": 421} {
		if res := hz.do(req{method: "GET", path: "/", header: map[string]string{"Host": host}}); res.StatusCode != want {
			t.Errorf("Host %s: %d, want %d", host, res.StatusCode, want)
		}
	}
}

func TestHeaders(t *testing.T) {
	hz := newHarness(t)
	tok, _ := hz.setup()
	for _, p := range []string{"/status", "/setup", "/login", "/assets/app.js", "/api/status", "/nope"} {
		res := hz.do(req{method: "GET", path: p, cookies: map[string]string{cookieSession: tok}})
		h := res.Header
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") ||
			!strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s CSP: %q", p, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") == "" {
			t.Errorf("%s headers: %v", p, h)
		}
		if !strings.HasPrefix(p, "/assets/") && h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s Cache-Control: %q", p, h.Get("Cache-Control"))
		}
	}
}

// Nothing secret, and nothing about who uses the relay, on any page.
func TestNoSecretsOnAnyPage(t *testing.T) {
	hz := newHarness(t)
	hz.s.o.Log.Warn("registration refused", "reason", "key", "key", secretKey, "from", "203.0.113.9")
	var pages []string
	for _, p := range []string{"/setup", "/login"} {
		res := hz.do(req{method: "GET", path: p})
		pages = append(pages, text(res))
	}
	tok, _ := hz.setup()
	for _, p := range []string{"/status", "/health", "/connect", "/settings", "/api/status"} {
		res := hz.do(req{method: "GET", path: p, cookies: map[string]string{cookieSession: tok}})
		pages = append(pages, text(res))
	}
	for i, body := range pages {
		for _, s := range []string{secretKey, secretMetrics, setupToken, password, "203.0.113.9", "$argon2id"} {
			if strings.Contains(body, s) {
				t.Errorf("page %d shows %q", i, s)
			}
		}
	}
	if !strings.Contains(pages[2], "srv-1c9e4a") || !strings.Contains(pages[3], "registration refused") {
		t.Fatal("the pages don't show what they should")
	}
}

func TestSettings(t *testing.T) {
	hz := newHarness(t)
	tok, csrf := hz.setup()
	hz.env["WITTYRELAY_CIRCUITS"] = "8"
	post := func(f url.Values) *http.Response {
		f.Set("csrf", csrf)
		return hz.do(req{method: "POST", path: "/settings", cookies: map[string]string{cookieSession: tok}, form: f})
	}
	// Locked by the environment: shown, not editable.
	page := text(hz.do(req{method: "GET", path: "/settings", cookies: map[string]string{cookieSession: tok}}))
	if !strings.Contains(page, "WITTYRELAY_CIRCUITS") {
		t.Fatal("the settings page doesn't say circuits is set in the environment")
	}
	res := post(url.Values{"kbps": {"6000"}, "circuits": {"99"}, "logLevel": {"debug"}, "name": {"Our relay"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/settings?saved=1" {
		t.Fatalf("save: %d %s", res.StatusCode, text(res))
	}
	b, _ := os.ReadFile(hz.path)
	var saved relay.Config
	_ = json.Unmarshal(b, &saved)
	if saved.Limits.Kbps != 6000 || saved.Name != "Our relay" || saved.Limits.Circuits != 8 || len(saved.Keys) != 1 {
		t.Fatalf("saved: %s", b)
	}
	if hz.level.Level() != slog.LevelDebug {
		t.Fatal("the log level didn't change at once")
	}
	page = text(hz.do(req{method: "GET", path: "/settings?saved=1", cookies: map[string]string{cookieSession: tok}}))
	if !strings.Contains(page, "Restart the relay") {
		t.Fatal("no restart notice after changing a limit")
	}
	for _, bad := range []url.Values{{"kbps": {"50"}}, {"kbps": {"lots"}}, {"servers": {"0"}}, {"logLevel": {"loud"}}, {"name": {strings.Repeat("x", 61)}}} {
		if res := post(bad); res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%v: %d", bad, res.StatusCode)
		}
	}
	if b2, _ := os.ReadFile(hz.path); string(b2) != string(b) {
		t.Fatal("a refused save changed the file")
	}
}

func TestSettingsFileReadOnly(t *testing.T) {
	hz := newHarness(t)
	tok, csrf := hz.setup()
	hz.s.o.ConfigPath = filepath.Join(hz.dir, "wittyrelay.json", "cannot-be-a-file-under-a-file.json")
	res := hz.do(req{method: "POST", path: "/settings", cookies: map[string]string{cookieSession: tok}, form: url.Values{"csrf": {csrf}, "kbps": {"6000"}}})
	if res.StatusCode != http.StatusConflict && res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unwritable file: %d", res.StatusCode)
	}
}

func TestPasswordChangeSignsOutOthers(t *testing.T) {
	hz := newHarness(t)
	tok, csrf := hz.setup()
	other := cookie(hz.login(password), cookieSession)
	post := func(cur, pw string) *http.Response {
		return hz.do(req{method: "POST", path: "/password", cookies: map[string]string{cookieSession: tok},
			form: url.Values{"csrf": {csrf}, "current": {cur}, "password": {pw}, "again": {pw}}})
	}
	if res := post("not it", "a new long password"); res.StatusCode != 401 {
		t.Fatalf("wrong current: %d", res.StatusCode)
	}
	if res := post(password, "short"); res.StatusCode != 422 {
		t.Fatalf("short new: %d", res.StatusCode)
	}
	if res := post(password, "a new long password"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("change: %d", res.StatusCode)
	}
	if hz.s.sess.get(other) != nil || hz.s.sess.get(tok) == nil {
		t.Fatal("other sessions should end, this one stay")
	}
	if res := hz.login(password); res.StatusCode != 401 {
		t.Fatalf("old password: %d", res.StatusCode)
	}
	if res := hz.login("a new long password"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("new password: %d", res.StatusCode)
	}
}

func TestSessionsExpire(t *testing.T) {
	hz := newHarness(t)
	tok, _ := hz.setup()
	hz.clock.add(sessionIdle - time.Minute)
	if hz.s.sess.get(tok) == nil {
		t.Fatal("ended before the idle time")
	}
	hz.clock.add(sessionIdle + time.Second)
	if hz.s.sess.get(tok) != nil {
		t.Fatal("still there after the idle time")
	}
	tok = cookie(hz.login(password), cookieSession)
	for range int(sessionMax/(sessionIdle/2)) + 1 {
		hz.clock.add(sessionIdle / 2)
		hz.s.sess.get(tok)
	}
	if hz.s.sess.get(tok) != nil {
		t.Fatal("still there after the longest a session lasts")
	}
	for range maxSessions + 5 {
		hz.s.sess.start()
	}
	if n := len(hz.s.sess.m); n > maxSessions {
		t.Fatalf("%d sessions kept", n)
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash: %s", h)
	}
	if !CheckPassword(h, "correct horse battery") || CheckPassword(h, "correct horse batterz") || CheckPassword(h, "") {
		t.Fatal("check")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("no salt")
	}
	for _, bad := range []string{"", "plain", strings.Replace(h, "argon2id", "argon2i", 1), strings.Replace(h, "m=19456", "m=99999999", 1),
		strings.Replace(h, "t=2", "t=0", 1), h[:len(h)-10]} {
		if CheckPassword(bad, "correct horse battery") {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestStatusAPI(t *testing.T) {
	hz := newHarness(t)
	tok, _ := hz.setup()
	for range 15 {
		hz.clock.add(sampleEvery)
		hz.s.Sample()
	}
	res := hz.do(req{method: "GET", path: "/api/status", cookies: map[string]string{cookieSession: tok}})
	var v statusView
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if !v.Up || v.Servers != "5" || v.ServersMax != "32" || v.Circuits != "7" || !strings.HasSuffix(v.Now, "Mbit/s") ||
		v.Spark == "" || len(v.Registered) != 5 || len(v.Refused) != 4 || v.LimitsHit != "20" || v.PerMinute == "0" {
		t.Fatalf("status: %+v", v)
	}
	if res.Header.Get("Content-Type") != "application/json" {
		t.Fatal(res.Header.Get("Content-Type"))
	}
}

func TestFormatting(t *testing.T) {
	for in, want := range map[float64]string{0: "0 Mbit/s", 850e3: "850 kbit/s", 12.4e6: "12.4 Mbit/s", 30e6: "30 Mbit/s", 1.25e9: "1.25 Gbit/s"} {
		if got := fmtBits(in); got != want {
			t.Errorf("fmtBits(%v) = %q", in, got)
		}
	}
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := num(in); got != want {
			t.Errorf("num(%d) = %q", in, got)
		}
	}
	for in, want := range map[time.Duration]string{30 * time.Second: "30 s", 5 * time.Minute: "5 min", 3*time.Hour + 4*time.Minute: "3 h 4 min",
		76 * time.Hour: "3 days 4 h"} {
		if got := fmtDur(in); got != want {
			t.Errorf("fmtDur(%v) = %q", in, got)
		}
	}
}
