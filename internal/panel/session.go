package panel

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"
)

// Session management of the control panel (ADR 0031). A browser gets a
// session only by redeeming a one-time sign-in code that `nakama ui`
// put into the URL it opened. Sessions live in this process's memory
// only: closing the panel ends them all.
const (
	// cookieName carries the session id. The __Host- prefix makes the
	// browser refuse the cookie unless it is Secure, host-only and on
	// Path=/, so a page on a sibling *.nakama.localhost host (all of them
	// are one site) cannot plant one (Chrome 158 and Firefox 160 store
	// Secure cookies on http://*.localhost, verified in the PoC of
	// Issue #139).
	cookieName = "__Host-nakama-panel"
	// signInCodeTTL is how long a sign-in code can be redeemed.
	signInCodeTTL = 60 * time.Second
	// sessionIdleTimeout ends a session not used for this long.
	sessionIdleTimeout = 30 * time.Minute
	// sessionMaxAge ends every session this long after the sign-in
	// (an AWS login session lasts at most 12 hours as well).
	sessionMaxAge = 12 * time.Hour
	// maxSessions and maxSignInCodes bound the memory a flood of
	// sign-ins could take; the oldest entries go first.
	maxSessions    = 32
	maxSignInCodes = 16
)

// session is one signed-in browser.
type session struct {
	id string
	// csrf is the token every state-changing request must carry.
	csrf     string
	created  time.Time
	lastSeen time.Time
}

// expiresAt is when the session ends at the latest.
func (s *session) expiresAt() time.Time { return s.created.Add(sessionMaxAge) }

// store holds the sign-in codes and the sessions.
type store struct {
	now    func() time.Time
	random io.Reader
	// maxCodes and maxSessions bound the pending codes and the sessions
	// (maxSignInCodes, maxSessions; tests lower them). A bound of zero or
	// less keeps only the newest entry.
	maxCodes, maxSessions int

	mu       sync.Mutex
	codes    map[string]time.Time // code -> expiry
	sessions map[string]*session
}

func newStore(now func() time.Time) *store {
	if now == nil {
		now = time.Now
	}
	return &store{now: now, random: rand.Reader, maxCodes: maxSignInCodes, maxSessions: maxSessions,
		codes: map[string]time.Time{}, sessions: map[string]*session{}}
}

func (s *store) randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(s.random, b); err != nil {
		return nil, fmt.Errorf("generate a random value: %w", err)
	}
	return b, nil
}

// newSignInCode mints a code (256 random bits, hex) that redeem turns
// into a session once, within signInCodeTTL.
func (s *store) newSignInCode() (string, error) {
	b, err := s.randomBytes(32)
	if err != nil {
		return "", err
	}
	code := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for c, exp := range s.codes {
		if !now.Before(exp) {
			delete(s.codes, c)
		}
	}
	for len(s.codes) > 0 && len(s.codes) >= s.maxCodes {
		oldest, first := "", time.Time{}
		for c, exp := range s.codes {
			if oldest == "" || exp.Before(first) {
				oldest, first = c, exp
			}
		}
		delete(s.codes, oldest)
	}
	s.codes[code] = now.Add(signInCodeTTL)
	return code, nil
}

// redeem consumes code and returns a new session; false when the code
// is unknown, used or expired.
func (s *store) redeem(code string) (*session, bool, error) {
	s.mu.Lock()
	exp, ok := s.codes[code]
	if ok {
		delete(s.codes, code)
	}
	now := s.now()
	s.mu.Unlock()
	if !ok || !now.Before(exp) {
		return nil, false, nil
	}
	id, err := s.randomBytes(32)
	if err != nil {
		return nil, false, err
	}
	csrf, err := s.randomBytes(32)
	if err != nil {
		return nil, false, err
	}
	sess := &session{
		id:       base64.RawURLEncoding.EncodeToString(id),
		csrf:     base64.RawURLEncoding.EncodeToString(csrf),
		created:  now,
		lastSeen: now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	for len(s.sessions) > 0 && len(s.sessions) >= s.maxSessions {
		var oldest *session
		for _, x := range s.sessions {
			if oldest == nil || x.created.Before(oldest.created) {
				oldest = x
			}
		}
		delete(s.sessions, oldest.id)
	}
	s.sessions[sess.id] = sess
	return sess, true, nil
}

// lookup returns the live session with id and marks it used.
func (s *store) lookup(id string) (*session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	now := s.now()
	if expired(sess, now) {
		delete(s.sessions, id)
		return nil, false
	}
	sess.lastSeen = now
	return sess, true
}

// end removes a session (sign-out, or a new sign-in in the same browser).
func (s *store) end(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *store) pruneLocked(now time.Time) {
	for id, x := range s.sessions {
		if expired(x, now) {
			delete(s.sessions, id)
		}
	}
}

func expired(s *session, now time.Time) bool {
	return !now.Before(s.lastSeen.Add(sessionIdleTimeout)) || !now.Before(s.expiresAt())
}
