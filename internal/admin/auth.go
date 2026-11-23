// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 WittyWilla and contributors

package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// The admin password is kept as an argon2id hash in admin.json in the data
// directory, next to the relay's key. It is read on every sign-in, so
// `wittyrelay admin-password` takes effect without a restart.

// argon2id at OWASP's recommended minimum (19 MiB, 2 passes): about 30 ms,
// and one at a time (hashSem), so sign-ins can't run the relay out of memory.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
	minPassword  = 10
	maxPassword  = 256
)

var hashSem = make(chan struct{}, 1)

// HashPassword is an argon2id hash in the PHC string format.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hashSem <- struct{}{}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	<-hashSem
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// CheckPassword says whether pw is the one hash was made from.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var v int
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	if m < 8*1024 || m > 256*1024 || t < 1 || t > 10 || p < 1 || p > 4 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[4])
	want, err2 := enc.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) < 16 || len(want) > 64 {
		return false
	}
	hashSem <- struct{}{}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	<-hashSem
	return subtle.ConstantTimeCompare(got, want) == 1
}

// CheckNewPassword says what is wrong with a password being set, if anything.
func CheckNewPassword(pw, again string) string {
	switch {
	case pw != again:
		return "The two passwords aren't the same."
	case len([]rune(pw)) < minPassword:
		return fmt.Sprintf("Use at least %d characters.", minPassword)
	case len(pw) > maxPassword:
		return fmt.Sprintf("Use at most %d characters.", maxPassword)
	}
	return ""
}

type passwordFile struct {
	Hash string    `json:"hash"`
	Set  time.Time `json:"set"`
}

// PasswordPath is where the hash is kept.
func PasswordPath(dataDir string) string { return filepath.Join(dataDir, "admin.json") }

func readHash(dataDir string) (string, error) {
	b, err := os.ReadFile(PasswordPath(dataDir))
	if err != nil {
		return "", err
	}
	var f passwordFile
	if err := json.Unmarshal(b, &f); err != nil || f.Hash == "" {
		return "", errors.New("admin.json isn't readable")
	}
	return f.Hash, nil
}

// SetPassword hashes pw and keeps the hash in the data directory.
func SetPassword(dataDir, pw string) error {
	h, err := HashPassword(pw)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(passwordFile{Hash: h, Set: time.Now().UTC()}, "", "  ")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dataDir, ".admin-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, PasswordPath(dataDir))
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand doesn't fail on any system Go supports
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func digest(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// session is one signed-in browser. The cookie holds a random token; only
// its hash is kept here, so a memory dump can't be replayed as a cookie.
type session struct {
	csrf    string
	created time.Time
	seen    time.Time
}

const (
	sessionIdle = 30 * time.Minute
	sessionMax  = 12 * time.Hour
	maxSessions = 32
)

type sessions struct {
	mu  sync.Mutex
	m   map[[32]byte]*session
	now func() time.Time
}

func newSessions(now func() time.Time) *sessions {
	return &sessions{m: map[[32]byte]*session{}, now: now}
}

// start makes a session, dropping the oldest when there are too many.
func (s *sessions) start() (token string, sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	if len(s.m) >= maxSessions {
		var oldest [32]byte
		var t time.Time
		for k, v := range s.m {
			if t.IsZero() || v.created.Before(t) {
				oldest, t = k, v.created
			}
		}
		delete(s.m, oldest)
	}
	token = randomToken(32)
	sess = &session{csrf: randomToken(32), created: now, seen: now}
	s.m[digest(token)] = sess
	return token, sess
}

// get is the live session for token, if any, and marks it seen.
func (s *sessions) get(token string) *session {
	if token == "" || len(token) > 100 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := digest(token)
	sess, ok := s.m[k]
	if !ok {
		return nil
	}
	now := s.now()
	if now.Sub(sess.seen) > sessionIdle || now.Sub(sess.created) > sessionMax {
		delete(s.m, k)
		return nil
	}
	sess.seen = now
	return sess
}

func (s *sessions) end(token string) {
	s.mu.Lock()
	delete(s.m, digest(token))
	s.mu.Unlock()
}

// endAllBut signs out every other browser, after a password change.
func (s *sessions) endAllBut(token string) {
	s.mu.Lock()
	keep := digest(token)
	for k := range s.m {
		if k != keep {
			delete(s.m, k)
		}
	}
	s.mu.Unlock()
}

func (s *sessions) sweepLocked(now time.Time) {
	for k, v := range s.m {
		if now.Sub(v.seen) > sessionIdle || now.Sub(v.created) > sessionMax {
			delete(s.m, k)
		}
	}
}

// limiter counts failed sign-ins per address and in all, in a sliding window.
// Too many and sign-in is refused, right password or not, until the window
// passes. The table is bounded: a flood of addresses fills the overall count
// instead.
type limiter struct {
	mu      sync.Mutex
	perKey  int
	overall int
	span    time.Duration
	tries   map[string][]time.Time
	all     []time.Time
	now     func() time.Time
}

func newLimiter(perKey, overall int, span time.Duration, now func() time.Time) *limiter {
	return &limiter{perKey: perKey, overall: overall, span: span, tries: map[string][]time.Time{}, now: now}
}

func (l *limiter) prune(list []time.Time, now time.Time) []time.Time {
	keep := list[:0]
	for _, t := range list {
		if now.Sub(t) < l.span {
			keep = append(keep, t)
		}
	}
	return keep
}

// blocked says whether key may not try now, and for how long.
func (l *limiter) blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.all = l.prune(l.all, now)
	list := l.prune(l.tries[key], now)
	if len(list) == 0 {
		delete(l.tries, key)
	} else {
		l.tries[key] = list
	}
	if len(list) >= l.perKey {
		return true, l.span - now.Sub(list[0])
	}
	if len(l.all) >= l.overall {
		return true, l.span - now.Sub(l.all[0])
	}
	return false, 0
}

func (l *limiter) failed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.tries) > 4096 {
		for k, list := range l.tries {
			if len(l.prune(list, now)) == 0 {
				delete(l.tries, k)
			}
		}
	}
	if _, ok := l.tries[key]; ok || len(l.tries) <= 4096 {
		l.tries[key] = append(l.tries[key], now)
	}
	l.all = append(l.all, now)
}

func (l *limiter) succeeded(key string) {
	l.mu.Lock()
	delete(l.tries, key)
	l.mu.Unlock()
}
