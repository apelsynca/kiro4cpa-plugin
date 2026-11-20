package kiro

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
)

func TestLoginStatusRestoresPublicAuthorizationDetails(t *testing.T) {
	state := newSession(t)
	updateLoginSession(state, func(s *LoginSession) {
		s.Method = "device-idc"
		s.VerifyURL = "https://view.awsapps.com/start"
		s.UserCode = "ABCD-EFGH"
		s.DeviceExpiresAt = time.Now().Add(time.Minute)
		s.Polling = true
	})
	status, body := callRoute(t, http.MethodGet, routeStatus, nil, url.Values{"state": {state}})
	if status != 200 || body["verificationUri"] != "https://view.awsapps.com/start" || body["userCode"] != "ABCD-EFGH" || body["deviceExpiresAt"] == nil {
		t.Fatalf("status=%d body=%v", status, body)
	}
	for _, key := range []string{"deviceCode", "clientId", "clientSecret"} {
		if _, ok := body[key]; ok {
			t.Fatalf("status exposed %s", key)
		}
	}
	if _, err := claimSession(state, "device-idc"); err == nil {
		t.Fatal("active device flow can be duplicated")
	}
}

func TestSocialStatusRestoresPKCELinkWithoutVerifier(t *testing.T) {
	state := newSession(t)
	status, body := callRoute(t, http.MethodPost, routeSocialStart, map[string]string{"state": state}, nil)
	if status != 200 {
		t.Fatal(status)
	}
	_, restored := callRoute(t, http.MethodGet, routeStatus, nil, url.Values{"state": {state}})
	if restored["url"] != body["url"] {
		t.Fatal("reload changed authorization URL")
	}
	session, _ := getLoginSession(state)
	raw, _ := json.Marshal(restored)
	if strings.Contains(string(raw), session.CodeVerifier) {
		t.Fatal("verifier leaked")
	}
}

func TestImportReportsInvalidCredentialsWithoutRetryingRefresh(t *testing.T) {
	calls := 0
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		calls++
		return &hostapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"error":"invalid_grant"}`)}, nil
	})
	state := newSession(t)
	status, body := callRoute(t, http.MethodPost, routeImport, map[string]string{"state": state, "method": "social", "refreshToken": "invalid-test"}, nil)
	if status != 200 || body["validation"] != "invalid" || body["warning"] == "" {
		t.Fatalf("%d %v", status, body)
	}
	s, _ := getLoginSession(state)
	buildLoginSuccessResponse(*s.Completed, state)
	if calls != 1 {
		t.Fatalf("invalid credential retried %d times", calls)
	}
}

func TestImportRetainsRotatedTokensAndReportsVerified(t *testing.T) {
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return ok(`{"accessToken":"fresh","refreshToken":"rotated","expiresIn":3600}`)
	})
	state := newSession(t)
	status, body := callRoute(t, http.MethodPost, routeImport, map[string]string{"state": state, "method": "social", "refreshToken": "test-token"}, nil)
	s, _ := getLoginSession(state)
	if status != 200 || body["validation"] != "valid" || s.Completed.RefreshToken != "rotated" {
		t.Fatalf("%d %v", status, body)
	}
}

func TestClaimSessionRejectsConcurrentActionAndReleasesAfterError(t *testing.T) {
	state := newSession(t)
	if _, err := claimSession(state, "social"); err != nil {
		t.Fatal(err)
	}
	if _, err := claimSession(state, "social"); err == nil {
		t.Fatal("duplicate action accepted")
	}
	releaseSession(state)
	if _, err := claimSession(state, "social"); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedSocialStartKeepsOriginalAuthorizationLink(t *testing.T) {
	state := newSession(t)
	_, first := callRoute(t, http.MethodPost, routeSocialStart, map[string]string{"state": state}, nil)
	_, second := callRoute(t, http.MethodPost, routeSocialStart, map[string]string{"state": state}, nil)
	if first["url"] != second["url"] {
		t.Fatal("repeated start invalidated PKCE link")
	}
}
