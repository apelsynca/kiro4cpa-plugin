package kiro

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// LoginSession tracks one CPA login attempt from method selection to a saved
// credential. The connect page drives it through management routes.
type LoginSession struct {
	State     string
	ExpiresAt time.Time
	// Method is the login method the user picked; empty until they choose.
	Method string

	// Device flow (Builder ID / IdC).
	UserCode        string
	VerifyURL       string
	Region          string
	StartURL        string
	Polling         bool
	Busy            bool
	DeviceExpiresAt time.Time

	// Social (Google / GitHub) PKCE flow.
	CodeVerifier string
	SocialState  string

	Completed  *Credential
	Message    string
	Error      string
	Validation string
	Warning    string
	Delivered  bool
}

var (
	loginSessionsMu sync.RWMutex
	loginSessions   = make(map[string]*LoginSession)
)

func createLoginSession(ttl time.Duration) *LoginSession {
	b := make([]byte, 24)
	rand.Read(b)
	state := hex.EncodeToString(b)
	now := time.Now().UTC()
	session := &LoginSession{
		State:     state,
		ExpiresAt: now.Add(ttl),
	}

	loginSessionsMu.Lock()
	defer loginSessionsMu.Unlock()
	cleanupLoginSessionsLocked(now)
	loginSessions[state] = session
	return session
}

// getLoginSession returns a copy, so callers never read fields that a
// background poller is writing under the lock.
func getLoginSession(state string) (*LoginSession, bool) {
	loginSessionsMu.RLock()
	defer loginSessionsMu.RUnlock()
	s, ok := loginSessions[state]
	if !ok || time.Now().UTC().After(s.ExpiresAt) {
		return nil, false
	}
	snapshot := *s
	return &snapshot, true
}

func completeLoginSession(state string, cred *Credential, message string) {
	updateLoginSession(state, func(s *LoginSession) {
		s.Completed = cred
		s.Message = message
		s.Error = ""
		s.Polling = false
		s.Busy = false
		s.CodeVerifier = ""
	})
}

func failLoginSession(state string, errMessage string) {
	updateLoginSession(state, func(s *LoginSession) {
		s.Error = errMessage
		s.Polling = false
		s.Busy = false
	})
}

func updateLoginSession(state string, fn func(s *LoginSession)) bool {
	loginSessionsMu.Lock()
	defer loginSessionsMu.Unlock()
	s, ok := loginSessions[state]
	if !ok || time.Now().UTC().After(s.ExpiresAt) {
		return false
	}
	fn(s)
	return true
}

func cleanupLoginSessionsLocked(now time.Time) {
	for k, v := range loginSessions {
		if now.After(v.ExpiresAt) {
			delete(loginSessions, k)
		}
	}
}
