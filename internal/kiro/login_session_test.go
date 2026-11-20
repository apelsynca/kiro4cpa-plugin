package kiro

import (
	"testing"
	"time"
)

func TestLoginSessionLifecycle(t *testing.T) {
	session := createLoginSession(time.Minute)
	if len(session.State) < 16 {
		t.Fatalf("session state too short: %q", session.State)
	}

	got, ok := getLoginSession(session.State)
	if !ok || got.State != session.State {
		t.Fatalf("getLoginSession failed to find active session")
	}

	cred := &Credential{AuthMethod: "api_key", KiroAPIKey: "test-key"}
	completeLoginSession(session.State, cred, "connected")

	got, _ = getLoginSession(session.State)
	if got.Completed == nil || got.Completed.KiroAPIKey != "test-key" {
		t.Fatalf("expected completed credential to match")
	}

	updateLoginSession(session.State, func(s *LoginSession) { s.ExpiresAt = time.Now().Add(-time.Minute) })
	if _, ok := getLoginSession(session.State); ok {
		t.Fatal("expected expired session to be unavailable")
	}
	createLoginSession(time.Minute)
	loginSessionsMu.RLock()
	_, present := loginSessions[session.State]
	loginSessionsMu.RUnlock()
	if present {
		t.Fatal("creating a session did not prune the expired session")
	}
}
