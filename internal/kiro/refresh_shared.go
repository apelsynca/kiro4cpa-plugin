package kiro

import (
	"crypto/sha256"
	"errors"
	"slices"
	"sync"
	"time"
)

const refreshHistoryLimit = 64

// Serializes refreshes and plugin writes per account. Host-owned writes happen
// after auth.refresh returns. History keeps fingerprints only;
// only the current credential retains its tokens. This state is process-local.
type refreshCoordinator struct {
	mu       sync.Mutex
	inflight map[string]*refreshCall
	latest   map[string]refreshResult
}

var refreshes = refreshCoordinator{
	inflight: map[string]*refreshCall{},
	latest:   map[string]refreshResult{},
}

type refreshKey [sha256.Size]byte

type refreshCall struct {
	key  refreshKey
	done chan struct{}
	cred Credential
	err  error
}

type refreshResult struct {
	to          Credential
	history     []refreshKey
	pruned      bool
	persisted   bool
	savePending bool
}

var (
	errRefreshAborted  = errors.New("kiro refresh aborted")
	errStaleCredential = errors.New("kiro credential snapshot is too old; reload the current credential or sign in again")
)

func keyOf(c Credential) refreshKey {
	return sha256.Sum256([]byte(c.AccessToken + "\x00" + c.RefreshToken))
}

func (r *refreshResult) remember(key refreshKey) {
	if slices.Contains(r.history, key) {
		return
	}
	r.history = append(r.history, key)
	if len(r.history) > refreshHistoryLimit {
		r.history = r.history[1:]
		r.pruned = true
	}
}

func sharedRefresh(authID string, c Credential, refresh func(Credential) (Credential, error)) (Credential, error) {
	return coordinatedRefresh(authID, c, refresh, nil)
}

func hasPendingRefreshSave(authID string, c Credential) bool {
	refreshes.mu.Lock()
	defer refreshes.mu.Unlock()
	last, ok := refreshes.latest[authID]
	key := keyOf(c)
	return ok && last.savePending && (key == keyOf(last.to) || slices.Contains(last.history, key))
}

// A save failure does not discard rotated tokens. The next executor/quota caller
// retries saving the cached result without spending the old refresh token again.
func coordinatedRefresh(authID string, c Credential, refresh func(Credential) (Credential, error), persist func(Credential) error) (Credential, error) {
	return coordinateRefresh(authID, c, refresh, persist, false)
}

// repairSave retries persistence before a request. A token rejected by the
// upstream must instead refresh even if its last save is still pending.
func coordinateRefresh(authID string, c Credential, refresh func(Credential) (Credential, error), persist func(Credential) error, repairSave bool) (Credential, error) {
	if authID == "" || c.AuthMethod == AuthMethodAPIKey {
		fresh, err := refresh(c)
		if err == nil && persist != nil {
			_ = persist(fresh)
		}
		return fresh, err
	}
	key := keyOf(c)
	for {
		refreshes.mu.Lock()
		if call, ok := refreshes.inflight[authID]; ok {
			refreshes.mu.Unlock()
			<-call.done
			if call.key == key && call.err != nil {
				return call.cred, call.err
			}
			continue
		}
		last, exists := refreshes.latest[authID]
		current := exists && key == keyOf(last.to)
		knownOld := exists && !current && slices.Contains(last.history, key)
		if repairSave && !current && !knownOld {
			refreshes.mu.Unlock()
			return c, nil
		}
		reuse := knownOld || (repairSave && current)
		if reuse {
			// Only tokens belong to the cache; retain the caller's current region,
			// pacing, identity and other account settings.
			c.AccessToken, c.RefreshToken = last.to.AccessToken, last.to.RefreshToken
			c.ExpiresAt, c.ProfileArn = last.to.ExpiresAt, last.to.ProfileArn
			// An old snapshot may outlive the replacement access token. Renew the
			// latest token in that case, never the old snapshot's refresh token.
			if expiry, err := time.Parse(time.RFC3339, c.ExpiresAt); err == nil && !expiry.After(time.Now()) {
				reuse = false
			}
		} else if exists && last.pruned && key != keyOf(last.to) {
			// Once fingerprints are retired, unknown older snapshots fail closed.
			// A newly signed-in credential with a later expiry can still replace it.
			expiry, err := time.Parse(time.RFC3339, c.ExpiresAt)
			latestExpiry, latestErr := time.Parse(time.RFC3339, last.to.ExpiresAt)
			if err != nil || latestErr != nil || !expiry.After(latestExpiry) {
				refreshes.mu.Unlock()
				return c, errStaleCredential
			}
		}
		if reuse && (persist == nil || last.persisted) {
			refreshes.mu.Unlock()
			return c, nil
		}
		call := &refreshCall{key: key, done: make(chan struct{}), err: errRefreshAborted}
		refreshes.inflight[authID] = call
		refreshes.mu.Unlock()
		return runRefresh(authID, c, last, exists, reuse, call, refresh, persist)
	}
}

func runRefresh(authID string, c Credential, result refreshResult, exists, reuse bool, call *refreshCall, refresh func(Credential) (Credential, error), persist func(Credential) error) (Credential, error) {
	// Copy before appending: readers may still hold a previous result's slice.
	result.history = slices.Clone(result.history)
	updated := false
	defer func() {
		refreshes.mu.Lock()
		if updated {
			refreshes.latest[authID] = result
		}
		delete(refreshes.inflight, authID)
		refreshes.mu.Unlock()
		close(call.done)
	}()
	if reuse {
		result.to = c
		call.cred = c
	} else {
		fresh, err := refresh(c)
		if err != nil {
			call.cred, call.err = fresh, err
			return fresh, err
		}
		if exists {
			result.remember(keyOf(result.to))
		}
		result.remember(keyOf(c))
		result.to, result.persisted = fresh, false
		result.savePending = false
		call.cred = fresh
	}
	updated = true
	if persist != nil && !result.persisted {
		result.persisted = persist(result.to) == nil
		result.savePending = !result.persisted
	}
	call.err = nil
	return call.cred, nil
}
