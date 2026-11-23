// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wittywilla/wittyrelay/internal/relay"
)

// page is what every template gets.
type page struct {
	Title  string
	Nav    string
	CSRF   string
	Signed bool
	Name   string
	Up     bool
	Ver    string
	Flash  string
	Error  string
	Secure bool // the browser will keep the session cookie
	Data   any
}

var pageNames = []string{"setup", "login", "status", "health", "connect", "settings"}

func parseTemplates() (map[string]*template.Template, error) {
	funcs := template.FuncMap{"join": strings.Join}
	out := map[string]*template.Template{}
	for _, name := range pageNames {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(webFS, "web/layout.html", "web/"+name+".html")
		if err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, code int, p page) {
	snap := s.src.Snapshot()
	p.Name, p.Up, p.Ver = s.o.Config.Name, snap.Ready, snap.Version
	p.Secure = secureContext(r)
	if cur := current(r); cur.sess != nil {
		p.Signed, p.CSRF = true, cur.sess.csrf
	}
	var buf bytes.Buffer
	if err := s.tpl[name].Execute(&buf, p); err != nil {
		s.o.Log.Error("the admin page couldn't be drawn", "page", name, "err", err)
		http.Error(w, "This page couldn't be drawn.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieSession); err == nil && s.sess.get(c.Value) != nil {
		http.Redirect(w, r, "/status", http.StatusSeeOther)
		return
	}
	if s.SetupToken() != "" {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- first password ----

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if s.SetupToken() == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, "setup", http.StatusOK, page{Title: "Set the admin password", CSRF: preToken(w, r)})
}

func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	want := s.SetupToken()
	if want == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "That form is too big.", http.StatusRequestEntityTooLarge)
		return
	}
	if !preTokenOK(r) {
		http.Error(w, "This form has expired. Go back, reload the page and try again.", http.StatusForbidden)
		return
	}
	fail := func(code int, msg string) {
		s.render(w, r, "setup", code, page{Title: "Set the admin password", CSRF: preToken(w, r), Error: msg})
	}
	if !secureContext(r) {
		fail(http.StatusForbidden, insecureMessage)
		return
	}
	who := clientKey(r)
	if no, wait := s.lim.blocked(who); no {
		fail(http.StatusTooManyRequests, tooMany(wait))
		return
	}
	if !tokenEqual(strings.TrimSpace(r.PostForm.Get("token")), want) {
		s.lim.failed(who)
		s.o.Log.Warn("admin setup: wrong setup token", "reason", "setup-token")
		fail(http.StatusUnauthorized, "That isn't the setup token. It's in the relay's log: docker compose logs wittyrelay | grep \"setup token\"")
		return
	}
	pw := r.PostForm.Get("password")
	if msg := CheckNewPassword(pw, r.PostForm.Get("again")); msg != "" {
		fail(http.StatusUnprocessableEntity, msg)
		return
	}
	s.mu.Lock()
	if s.setup == "" {
		s.mu.Unlock()
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	err := SetPassword(s.o.DataDir, pw)
	if err == nil {
		s.setup = ""
	}
	s.mu.Unlock()
	if err != nil {
		s.o.Log.Error("the admin password couldn't be kept", "err", err)
		fail(http.StatusInternalServerError, "The password couldn't be saved in the data directory. Is /data writable?")
		return
	}
	s.lim.succeeded(who)
	s.o.Log.Info("the admin password is set")
	s.startSession(w)
	http.Redirect(w, r, "/connect?welcome=1", http.StatusSeeOther)
}

// ---- sign in and out ----

const insecureMessage = "Signing in works only over HTTPS or from this machine (localhost). Open it through an SSH tunnel or a reverse proxy with HTTPS; README, \"Admin page\"."

func tooMany(wait time.Duration) string {
	m := int(wait.Minutes()) + 1
	return fmt.Sprintf("Too many wrong tries. Wait %d minute%s and try again.", m, map[bool]string{true: "", false: "s"}[m == 1])
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.SetupToken() != "" {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if c, err := r.Cookie(cookieSession); err == nil && s.sess.get(c.Value) != nil {
		http.Redirect(w, r, "/status", http.StatusSeeOther)
		return
	}
	p := page{Title: "Sign in", CSRF: preToken(w, r)}
	if r.URL.Query().Has("out") {
		p.Flash = "You're signed out."
	}
	s.render(w, r, "login", http.StatusOK, p)
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "That form is too big.", http.StatusRequestEntityTooLarge)
		return
	}
	if !preTokenOK(r) {
		http.Error(w, "This form has expired. Go back, reload the page and try again.", http.StatusForbidden)
		return
	}
	fail := func(code int, msg string) {
		s.render(w, r, "login", code, page{Title: "Sign in", CSRF: preToken(w, r), Error: msg})
	}
	if !secureContext(r) {
		fail(http.StatusForbidden, insecureMessage)
		return
	}
	who := clientKey(r)
	if no, wait := s.lim.blocked(who); no {
		fail(http.StatusTooManyRequests, tooMany(wait))
		return
	}
	hash, err := readHash(s.o.DataDir)
	if err != nil {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if !CheckPassword(hash, r.PostForm.Get("password")) {
		s.lim.failed(who)
		s.o.Log.Warn("admin sign-in: wrong password", "reason", "password")
		fail(http.StatusUnauthorized, "That isn't the admin password.")
		return
	}
	s.lim.succeeded(who)
	s.startSession(w)
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.sess.end(current(r).token)
	setCookie(w, cookieSession, "", -1)
	http.Redirect(w, r, "/login?out=1", http.StatusSeeOther)
}

// ---- status ----

type refusal struct {
	Label string `json:"label"`
	Count string `json:"count"`
}

type server struct {
	ID      string `json:"id"`
	Devices string `json:"devices"`
	Renews  string `json:"renews"`
}

// limitRow is one per-network limit: what it allows, the busiest network
// or address now, and how many are at the limit. Numbers only.
type limitRow struct {
	Label   string `json:"label"`
	Allowed string `json:"allowed"`
	Busiest string `json:"busiest"`
	AtLimit string `json:"atLimit"`
}

type statusView struct {
	Up bool `json:"up"`
	// Health is what servers are told (relay.Health), in words.
	Health      string     `json:"health"`
	HealthState string     `json:"healthState"`
	HealthLoad  string     `json:"healthLoad"`
	HealthNote  string     `json:"healthNote"`
	Conns       string     `json:"conns"`
	ConnsMax    string     `json:"connsMax"`
	ConnsDetail string     `json:"connsDetail"`
	Memory      string     `json:"memory"`
	Limits      []limitRow `json:"limits"`
	State       string     `json:"state"`
	Version     string     `json:"version"`
	Uptime      string     `json:"uptime"`
	Started     string     `json:"started"`
	Servers     string     `json:"servers"`
	ServersMax  string     `json:"serversMax"`
	Places      string     `json:"places"`
	Circuits    string     `json:"circuits"`
	CircuitsMax string     `json:"circuitsMax"`
	Opened      string     `json:"opened"`
	PerMinute   string     `json:"perMinute"`
	Now         string     `json:"now"`
	Peak        string     `json:"peak"`
	PeakAt      string     `json:"peakAt"`
	Cap         string     `json:"cap"`
	Relayed     string     `json:"relayed"`
	Spark       string     `json:"spark"`
	SparkTop    string     `json:"sparkTop"`
	LimitsHit   string     `json:"limitsHit"`
	Refused     []refusal  `json:"refused"`
	Registered  []server   `json:"registered"`
}

// refusalLabels say what each refusal reason means to whoever runs the relay.
var refusalLabels = map[string]string{
	"busy":                   "Too many registrations from one address in a minute",
	"key":                    "Registration without a right access key",
	"full":                   "Registration refused: relay full, or too many servers from one address",
	"bad":                    "Registration the relay couldn't read",
	"reserve":                "Place asked for by a server that isn't registered",
	"connect":                "Device its server didn't list",
	"circuit-limit":          "Connection refused: a server's connection limit",
	"reservation-limit":      "Place refused: the relay's place limit",
	"circuit-refused":        "Connection refused",
	"reservation-refused":    "Place refused",
	"circuit-no-reservation": "Connection to a server holding no place",
	"circuit-failed":         "Connection the server didn't answer",
	"circuit-malformed":      "Connection request the relay couldn't read",
	"reservation-malformed":  "Place request the relay couldn't read",
}

func (s *Server) statusView() statusView {
	snap := s.src.Snapshot()
	rt := s.hist.rates()
	now := s.o.Now()
	o := snap.Offer
	v := statusView{Up: snap.Ready, State: map[bool]string{true: "Up", false: "Starting"}[snap.Ready],
		Version: snap.Version, Uptime: fmtDur(now.Sub(snap.Started)), Started: snap.Started.UTC().Format("2006-01-02 15:04 UTC"),
		Servers: num(snap.Servers), ServersMax: num(int64(o.Servers)), Places: num(snap.Reservations),
		Circuits: num(snap.Circuits), CircuitsMax: num(int64(o.Servers * o.Circuits)),
		Opened: num(snap.CircuitsTotal), PerMinute: num(rt.PerMinute),
		Now: fmtBits(rt.Now), Peak: fmtBits(rt.Peak), Relayed: fmtBytes(snap.BytesTotal),
		Spark: sparkline(rt.Rates, 600, 120, 119), SparkTop: fmtBits(maxOf(rt.Rates))}
	if !rt.PeakAt.IsZero() {
		v.PeakAt = "at " + rt.PeakAt.UTC().Format("15:04 UTC") + ", " + fmtDur(now.Sub(rt.PeakAt)) + " ago"
	} else {
		v.PeakAt = "nothing relayed yet"
	}
	if o.TotalKbps > 0 {
		v.Cap = "Cap: " + fmtBits(float64(o.TotalKbps)*1000) + " each way"
	} else {
		v.Cap = "No overall cap"
	}
	var hit int64
	reasons := make([]string, 0, len(snap.Refused))
	for k := range snap.Refused {
		// libp2p reports what the relay's own list refused ("connect",
		// "reserve") once more as "denied": count each refusal once.
		if k == "circuit-denied" || k == "reservation-denied" {
			continue
		}
		reasons = append(reasons, k)
	}
	sort.Slice(reasons, func(i, j int) bool {
		a, b := snap.Refused[reasons[i]], snap.Refused[reasons[j]]
		return a > b || (a == b && reasons[i] < reasons[j])
	})
	for _, k := range reasons {
		n := snap.Refused[k]
		hit += n
		label := refusalLabels[k]
		if label == "" {
			label = k
		}
		v.Refused = append(v.Refused, refusal{Label: label, Count: num(n)})
	}
	v.LimitsHit = num(hit)
	v.Health, v.HealthNote = healthWords(snap.Health.State)
	v.HealthState, v.HealthLoad = snap.Health.State, strconv.Itoa(snap.Health.Load)+"%"
	c := snap.Conns
	v.Conns, v.ConnsMax = num(int64(c.Open)), num(int64(c.Budget))
	v.ConnsDetail = fmt.Sprintf("%s in, %s out; %s still saying who they are (of %s)",
		num(int64(c.Inbound)), num(int64(c.Outbound)), num(int64(c.Unidentified)), num(int64(c.UnidentifiedMax)))
	if c.MemoryMax > 0 {
		v.Memory = fmt.Sprintf("%.1f MiB of %d MiB", c.MemoryMiB, c.MemoryMax)
	}
	v.Limits = limitRows(snap.Networks, snap.Refused["busy"])
	for _, r := range snap.Registered {
		v.Registered = append(v.Registered, server{ID: r.ID, Devices: num(int64(r.Devices)), Renews: fmtDur(r.Until.Sub(now))})
	}
	if v.Refused == nil {
		v.Refused = []refusal{}
	}
	if v.Registered == nil {
		v.Registered = []server{}
	}
	return v
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "status", http.StatusOK, page{Title: "Status", Nav: "status", Data: s.statusView()})
}

func (s *Server) apiStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.statusView())
}

// ---- health ----

type check struct {
	State  string // ok | warn | bad | info
	Title  string
	Detail string
}

type port struct {
	Port   string
	Proto  string
	Use    string
	Public bool
}

type healthView struct {
	Checks []check
	ID     string
	Addrs  []string
	Ports  []port
	Errors []logLine
}

type logLine struct {
	Time    string
	Level   string
	Message string
	Detail  string
}

func (s *Server) ports() []port {
	c := s.o.Config
	p := strconv.Itoa(c.Port)
	out := []port{
		{p, "UDP", "QUIC: what servers and apps use first", true},
		{p, "TCP", "For networks that let no UDP out", true},
	}
	if c.HTTP != "" {
		out = append(out, port{portOf(c.HTTP), "TCP", "Health, metrics and /info: keep it to this machine", false})
	}
	if c.Admin.Listen != "" {
		out = append(out, port{portOf(c.Admin.Listen), "TCP", "This admin page: keep it to this machine", false})
	}
	return out
}

func portOf(hostport string) string {
	if _, p, err := net.SplitHostPort(hostport); err == nil {
		return p
	}
	return hostport
}

func (s *Server) healthPage(w http.ResponseWriter, r *http.Request) {
	snap := s.src.Snapshot()
	c := s.o.Config
	var checks []check
	if snap.Ready {
		checks = append(checks, check{"ok", "Listening", fmt.Sprintf("On port %d, UDP (QUIC) and TCP.", c.Port)})
	} else {
		checks = append(checks, check{"bad", "Not listening yet", "The relay hasn't opened its port. The log says why."})
	}
	switch {
	case len(c.Announce) > 0:
		checks = append(checks, check{"ok", "Public address set", "Servers are told the addresses below."})
	case c.AllowPrivate:
		checks = append(checks, check{"info", "No public address set", "Fine for a relay on a home or office network. For the internet, set WITTYRELAY_ANNOUNCE."})
	default:
		checks = append(checks, check{"warn", "No public address set", "Set WITTYRELAY_ANNOUNCE to your domain or public IP. Without it servers are told what this machine sees, which in Docker or behind a router is a private address."})
	}
	switch word, note := healthWords(snap.Health.State); snap.Health.State {
	case "ok":
		checks = append(checks, check{"ok", "Servers are told: " + word, fmt.Sprintf("Load %d%%. %s", snap.Health.Load, note)})
	case "busy", "full":
		checks = append(checks, check{"warn", "Servers are told: " + word, fmt.Sprintf("Load %d%%. %s Raise the limits in Settings if this machine has room.", snap.Health.Load, note)})
	}
	if c.Access == "open" {
		checks = append(checks, check{"info", "Open to any server", "Any server may register. Each is still limited, and only reaches devices it lists."})
	} else {
		checks = append(checks, check{"ok", "Access keys required", fmt.Sprintf("%d access key%s set. Only servers given one can register.", len(c.Keys), plural(len(c.Keys)))})
	}
	if path := s.o.ConfigPath; path != "" {
		if writable(filepath.Dir(path)) {
			checks = append(checks, check{"ok", "Settings can be saved", "Saved to " + path + "."})
		} else {
			checks = append(checks, check{"warn", "Settings can't be saved", path + " can't be written. Change settings there or in the environment."})
		}
	}
	if c.MetricsToken == "" && c.HTTP != "" && !loopbackListen(c.HTTP) {
		checks = append(checks, check{"info", "Metrics need no token", "Anyone who reaches port " + portOf(c.HTTP) + " can read them. Keep that port to this machine, or set WITTYRELAY_METRICS_TOKEN."})
	}
	if pending, err := s.restartWaiting(); err != nil {
		checks = append(checks, check{"bad", "The configuration file has a problem", "At the next start the relay would refuse it: " + err.Error()})
	} else if pending {
		checks = append(checks, check{"warn", "Restart to apply saved settings", "docker compose restart wittyrelay"})
	}
	recent := s.o.Ring.Recent()
	hour := 0
	for _, e := range recent {
		if s.o.Now().Sub(e.Time) < time.Hour {
			hour++
		}
	}
	if hour == 0 {
		checks = append(checks, check{"ok", "No warnings in the last hour", ""})
	} else {
		checks = append(checks, check{"warn", fmt.Sprintf("%d warning%s or error%s in the last hour", hour, plural(hour), plural(hour)), "Listed below."})
	}
	v := healthView{Checks: checks, ID: snap.ID, Addrs: snap.Addrs, Ports: s.ports()}
	for _, e := range recent {
		d := e.Reason
		if e.Err != "" {
			d = strings.TrimSpace(d + " " + e.Err)
		}
		v.Errors = append(v.Errors, logLine{Time: e.Time.UTC().Format("Jan 2 15:04:05"), Level: strings.ToLower(e.Level), Message: e.Message, Detail: d})
	}
	s.render(w, r, "health", http.StatusOK, page{Title: "Health", Nav: "health", Data: v})
}

func loopbackListen(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".wittyrelay-check-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}

// ---- connect a server ----

type connectView struct {
	Welcome bool
	ID      string
	Addrs   []string
	Access  string
	Ports   []port
	Compose string
	Name    string
}

const composeSnippet = `services:
  wittyrelay:
    image: ghcr.io/wittywilla/wittyrelay:latest
    restart: unless-stopped
    ports:
      - "4001:4001/udp"
      - "4001:4001/tcp"
      - "127.0.0.1:8080:8080"   # health and metrics: this machine only
      - "127.0.0.1:8081:8081"   # this admin page: this machine only
    volumes:
      - wittyrelay-data:/data
    environment:
      WITTYRELAY_ANNOUNCE: "/dns4/relay.example.com/udp/4001/quic-v1,/dns4/relay.example.com/tcp/4001"
      WITTYRELAY_ACCESS: "key"
      WITTYRELAY_KEYS: "make-one-with-wittyrelay-key"
      WITTYRELAY_ADMIN: ":8081"
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
volumes:
  wittyrelay-data:`

func (s *Server) connectPage(w http.ResponseWriter, r *http.Request) {
	snap := s.src.Snapshot()
	v := connectView{Welcome: r.URL.Query().Has("welcome"), ID: snap.ID, Addrs: snap.Addrs, Access: s.o.Config.Access,
		Ports: s.ports(), Compose: composeSnippet, Name: s.o.Config.Name}
	s.render(w, r, "connect", http.StatusOK, page{Title: "Connect a server", Nav: "connect", Data: v})
}

// ---- settings ----

type field struct {
	Name, Label, Hint, Unit string
	Value                   string
	Kind                    string // number | text | textarea | select
	Min, Max                string
	MaxLen                  int
	Options                 []string
	Env                     string // set by this environment variable: shown, not editable
}

type group struct {
	Title, Hint string
	Fields      []field
}

type settingsView struct {
	Groups     []group
	ConfigPath string
	Pending    bool
	LoadErr    string
	Saved      bool
	Applied    bool
	PwError    string
	PwChanged  bool
}

func (s *Server) settingsFields(cur relay.Settings) []group {
	env := relay.SettingsFromEnv(s.o.Env)
	n := func(name, label, hint, unit string, v int64, min, max string) field {
		return field{Name: name, Label: label, Hint: hint, Unit: unit, Value: strconv.FormatInt(v, 10), Kind: "number", Min: min, Max: max, Env: env[name]}
	}
	return []group{
		{Title: "Bandwidth", Hint: "Told to every server before it uses the relay. 0 means no limit.", Fields: []field{
			n("kbps", "Speed per connection", "Each way. At least 100 when set.", "kbit/s", int64(cur.Kbps), "0", "10000000"),
			n("totalKbps", "Speed for everything together", "Each way, all connections at once.", "kbit/s", int64(cur.TotalKbps), "0", "100000000"),
			n("maxHeight", "Picture height streams keep to", "Servers send video no taller than this through the relay. 144 to 4320, or 0 for any.", "pixels", int64(cur.MaxHeight), "0", "4320"),
			n("dataMiB", "Data per connection", "Each way. Past it the device opens a new connection.", "MiB", cur.DataMiB, "0", "1048576"),
			n("minutes", "Time per connection", "Past it the device opens a new connection.", "minutes", int64(cur.Minutes), "0", "10080"),
		}},
		{Title: "Connections", Hint: "How many the relay takes at once.", Fields: []field{
			n("servers", "Servers", "Registered at once.", "servers", int64(cur.Servers), "1", "100000"),
			n("circuits", "Connections per server", "Devices connected to one server through the relay at once.", "connections", int64(cur.Circuits), "1", "1024"),
			n("devicesPerServer", "Devices a server may list", "Only listed devices reach a server.", "devices", int64(cur.DevicesPerServer), "1", "1024"),
		}},
		{Title: "Per address", Hint: "Against one address taking the whole relay.", Fields: []field{
			n("perIP", "Servers from one IP address", "", "servers", int64(cur.PerIP), "1", "100000"),
			n("registerPerMinute", "Registrations from one IP address", "Renewals count too.", "a minute", int64(cur.RegisterPerMinute), "1", "10000"),
		}},
		{Title: "About this relay", Hint: "Shown to server owners before they use it.", Fields: []field{
			{Name: "name", Label: "Name", Kind: "text", Value: cur.Name, MaxLen: 60, Env: env["name"]},
			{Name: "note", Label: "Note", Hint: "Who runs it and for whom.", Kind: "textarea", Value: cur.Note, MaxLen: 280, Env: env["note"]},
		}},
		{Title: "Logging", Fields: []field{
			{Name: "logLevel", Label: "Log level", Hint: "Applies at once. Warnings and errors also show on Health.", Kind: "select",
				Value: cur.LogLevel, Options: []string{"debug", "info", "warn", "error"}, Env: env["logLevel"]},
		}},
	}
}

// restartWaiting says whether the relay would start with other settings than
// it runs with: saved here, or changed in the file by hand.
func (s *Server) restartWaiting() (bool, error) {
	next, err := s.o.Load()
	if err != nil {
		return false, err
	}
	a, b := relay.SettingsOf(next), relay.SettingsOf(s.o.Config)
	a.LogLevel, b.LogLevel = "", ""
	return a != b, nil
}

func (s *Server) settingsData(cur relay.Settings) settingsView {
	v := settingsView{Groups: s.settingsFields(cur), ConfigPath: s.o.ConfigPath}
	pending, err := s.restartWaiting()
	v.Pending = pending
	if err != nil {
		v.LoadErr = err.Error()
	}
	return v
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request) {
	cur := relay.SettingsOf(s.o.Config)
	if next, err := s.o.Load(); err == nil {
		cur = relay.SettingsOf(next)
	}
	cur.LogLevel = strings.ToLower(s.o.Level.Level().String())
	v := s.settingsData(cur)
	q := r.URL.Query()
	v.Saved, v.Applied, v.PwChanged = q.Has("saved"), q.Get("saved") == "now", q.Has("password")
	s.render(w, r, "settings", http.StatusOK, page{Title: "Settings", Nav: "settings", Data: v})
}

func (s *Server) settingsPost(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	cur := relay.SettingsOf(s.o.Config)
	if next, err := s.o.Load(); err == nil {
		cur = relay.SettingsOf(next)
	}
	cur.LogLevel = strings.ToLower(s.o.Level.Level().String())
	env := relay.SettingsFromEnv(s.o.Env)
	next := cur
	var bad []string
	num := func(name string, to *int) {
		if _, locked := env[name]; locked || !f.Has(name) {
			return
		}
		v, err := strconv.Atoi(strings.TrimSpace(f.Get(name)))
		if err != nil {
			bad = append(bad, name)
			return
		}
		*to = v
	}
	num("kbps", &next.Kbps)
	num("totalKbps", &next.TotalKbps)
	num("maxHeight", &next.MaxHeight)
	mib := int(next.DataMiB)
	num("dataMiB", &mib)
	next.DataMiB = int64(mib)
	num("minutes", &next.Minutes)
	num("servers", &next.Servers)
	num("circuits", &next.Circuits)
	num("perIP", &next.PerIP)
	num("registerPerMinute", &next.RegisterPerMinute)
	num("devicesPerServer", &next.DevicesPerServer)
	str := func(name string, to *string) {
		if _, locked := env[name]; !locked && f.Has(name) {
			*to = f.Get(name)
		}
	}
	str("name", &next.Name)
	str("note", &next.Note)
	str("logLevel", &next.LogLevel)

	fail := func(code int, msg string) {
		v := s.settingsData(next)
		s.render(w, r, "settings", code, page{Title: "Settings", Nav: "settings", Error: msg, Data: v})
	}
	if len(bad) > 0 {
		fail(http.StatusUnprocessableEntity, "Not a whole number: "+strings.Join(bad, ", ")+".")
		return
	}
	run, err := relay.SaveSettings(s.o.ConfigPath, next, s.o.Env)
	if err != nil {
		if errors.Is(err, relay.ErrReadOnly) {
			s.o.Log.Warn("admin settings: the configuration file can't be written", "err", err)
			fail(http.StatusConflict, "The configuration file can't be written: "+s.o.ConfigPath+". Change it there, or in the environment, and restart the relay.")
			return
		}
		fail(http.StatusUnprocessableEntity, "Not saved: "+err.Error()+".")
		return
	}
	s.o.Log.Info("admin settings saved", "path", s.o.ConfigPath)
	saved := "1"
	if lv := levelOf(run.LogLevel); lv != s.o.Level.Level() {
		s.o.Level.Set(lv)
		s.o.Log.Info("log level changed", "level", strings.ToLower(lv.String()))
	}
	if pending, _ := s.restartWaiting(); !pending {
		saved = "now"
	}
	http.Redirect(w, r, "/settings?saved="+saved, http.StatusSeeOther)
}

// LevelOf is the slog level a log level setting names.
func levelOf(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// LevelOf is levelOf for the command.
func LevelOf(s string) slog.Level { return levelOf(s) }

func (s *Server) passwordPost(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	cur := relay.SettingsOf(s.o.Config)
	if next, err := s.o.Load(); err == nil {
		cur = relay.SettingsOf(next)
	}
	cur.LogLevel = strings.ToLower(s.o.Level.Level().String())
	fail := func(code int, msg string) {
		v := s.settingsData(cur)
		v.PwError = msg
		s.render(w, r, "settings", code, page{Title: "Settings", Nav: "settings", Data: v})
	}
	who := clientKey(r)
	if no, wait := s.lim.blocked(who); no {
		fail(http.StatusTooManyRequests, tooMany(wait))
		return
	}
	hash, err := readHash(s.o.DataDir)
	if err != nil || !CheckPassword(hash, f.Get("current")) {
		s.lim.failed(who)
		s.o.Log.Warn("admin password change: wrong current password", "reason", "password")
		fail(http.StatusUnauthorized, "That isn't the current password.")
		return
	}
	pw := f.Get("password")
	if msg := CheckNewPassword(pw, f.Get("again")); msg != "" {
		fail(http.StatusUnprocessableEntity, msg)
		return
	}
	if err := SetPassword(s.o.DataDir, pw); err != nil {
		s.o.Log.Error("the admin password couldn't be kept", "err", err)
		fail(http.StatusInternalServerError, "The password couldn't be saved in the data directory.")
		return
	}
	s.lim.succeeded(who)
	s.sess.endAllBut(current(r).token)
	s.o.Log.Info("the admin password was changed; other browsers are signed out")
	http.Redirect(w, r, "/settings?password=1#password", http.StatusSeeOther)
}

// ---- formatting ----

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func maxOf(xs []float64) float64 {
	m := 0.0
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}

func num(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func fmtBits(bps float64) string {
	switch {
	case bps < 1:
		return "0 Mbit/s"
	case bps < 1e6:
		return fmt.Sprintf("%.0f kbit/s", bps/1e3)
	case bps < 1e9:
		return trimZero(fmt.Sprintf("%.1f", bps/1e6)) + " Mbit/s"
	}
	return trimZero(fmt.Sprintf("%.2f", bps/1e9)) + " Gbit/s"
}

func fmtBytes(n int64) string {
	f := float64(n)
	switch {
	case f < 1<<20:
		return trimZero(fmt.Sprintf("%.1f", f/(1<<10))) + " KiB"
	case f < 1<<30:
		return trimZero(fmt.Sprintf("%.1f", f/(1<<20))) + " MiB"
	case f < 1<<40:
		return trimZero(fmt.Sprintf("%.2f", f/(1<<30))) + " GiB"
	}
	return trimZero(fmt.Sprintf("%.2f", f/(1<<40))) + " TiB"
}

func trimZero(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func fmtDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d days %d h", int(d.Hours())/24, int(d.Hours())%24)
}

// healthWords say what each health state means, as the status page shows it.
func healthWords(state string) (word, note string) {
	switch state {
	case "ok":
		return "OK", "Room for more servers and connections."
	case "busy":
		return "Busy", "Most of what the relay allows is in use. Servers are told it's busy."
	case "full":
		return "Full", "No more servers can register. Those registered keep their place."
	case "starting":
		return "Starting", "The relay isn't listening yet."
	}
	return state, ""
}

// limitRows are the per-network limits, with the busiest network or address
// now. busy is how many registrations were refused for coming too often.
func limitRows(n relay.NetworkUse, busy int64) []limitRow {
	of := func(at, with int, what string) string {
		return fmt.Sprintf("%s of %s %s", num(int64(at)), num(int64(with)), what)
	}
	return []limitRow{
		{Label: "Servers from one network (an IPv4 address, or an IPv6 /64)", Allowed: num(int64(n.ServersMax)),
			Busiest: num(int64(n.BusiestServers)), AtLimit: of(n.ServersAtMax, n.WithServers, "networks with servers")},
		{Label: "Connections from one address (an IPv4 address, or an IPv6 /56)", Allowed: num(int64(n.ConnsMax)),
			Busiest: num(int64(n.BusiestConns)), AtLimit: of(n.ConnsAtMax, n.WithConns, "addresses connected")},
		{Label: "Registrations from one network a minute", Allowed: num(int64(n.RegisterPerMinute)),
			Busiest: "–", AtLimit: num(busy) + " refused since start"},
		{Label: "New connections from one address", Allowed: fmt.Sprintf("%s at once, then %d a second", num(int64(n.NewConnBurst)), n.NewConnPerSec),
			Busiest: "–", AtLimit: "–"},
	}
}
