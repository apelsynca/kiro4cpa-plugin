package kiro

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/config"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/wire"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// resetLimiter clears the process-wide limiter. Most executor tests share one
// auth ID, so parking and pacing state would otherwise leak between them.
func resetLimiter(t *testing.T) {
	t.Helper()
	clear := func() {
		limiter.mu.Lock()
		limiter.states = make(map[string]*credentialState)
		limiter.now = time.Now
		limiter.sleep = func(time.Duration) {}
		limiter.mu.Unlock()
	}
	clear()
	t.Cleanup(func() {
		clear()
		limiter.mu.Lock()
		limiter.sleep = time.Sleep
		limiter.mu.Unlock()
	})
}

// resetRefreshes forgets shared refresh results. Tests reuse one auth ID and
// one stored credential, so a refresh in one would otherwise answer the next.
func resetRefreshes(t *testing.T) {
	t.Helper()
	clear := func() {
		refreshes.mu.Lock()
		refreshes.inflight = map[string]*refreshCall{}
		refreshes.latest = map[string]refreshResult{}
		refreshes.mu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func stubHTTP(t *testing.T, fn func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error)) *[]hostapi.HTTPRequest {
	t.Helper()
	resetLimiter(t)
	resetRefreshes(t)
	var mu sync.Mutex
	var seen []hostapi.HTTPRequest
	orig := hostHTTPDo
	hostHTTPDo = func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
		return fn(r)
	}
	t.Cleanup(func() { hostHTTPDo = orig })
	return &seen
}

func ok(body string) (*hostapi.HTTPResponse, error) {
	return &hostapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}, nil
}

func TestRefreshSocialRequestAndRotation(t *testing.T) {
	seen := stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return ok(`{"accessToken":"at2","refreshToken":"rt2","expiresIn":3600}`)
	})
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	c := Credential{AuthMethod: AuthMethodSocial, AuthRegion: "us-east-1", RefreshToken: "rt-test", ProfileArn: "arn:keep"}
	got, err := refreshCredential(c, config.Get(), "cb-1", now)
	if err != nil {
		t.Fatal(err)
	}
	r := (*seen)[0]
	if r.URL != "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken" || r.HostCallbackID != "cb-1" || string(r.Body) != `{"refreshToken":"rt-test"}` {
		t.Fatalf("request %+v", r)
	}
	if ua := r.Headers["User-Agent"][0]; ua != "KiroIDE-"+config.Get().ClientVersion+"-0b57ce32592129d9e832f82147ef2a6cea350b58ca1e0596cfa331778daf69ff" {
		t.Fatalf("user-agent %s", ua)
	}
	if got.AccessToken != "at2" || got.RefreshToken != "rt2" || got.ProfileArn != "arn:keep" || got.ExpiresAt != "2026-10-20T13:00:00Z" {
		t.Fatalf("refreshed %+v", got)
	}
}

func TestRefreshKeepsTokenWhenNotRotatedAndDefaultsLifetime(t *testing.T) {
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) { return ok(`{"accessToken":"at2"}`) })
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	got, err := refreshCredential(Credential{AuthMethod: AuthMethodSocial, AuthRegion: "us-east-1", RefreshToken: "rt"}, config.Get(), "", now)
	if err != nil || got.RefreshToken != "rt" || got.ExpiresAt != "2026-10-20T13:00:00Z" {
		t.Fatalf("got %+v err=%v", got, err)
	}
}

func TestRefreshIDCAndExternalIDPRequests(t *testing.T) {
	seen := stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if strings.Contains(r.URL, "idp.example.com") {
			return ok(`{"access_token":"at3","expires_in":600}`)
		}
		return ok(`{"accessToken":"at2"}`)
	})
	cfg := config.Get()
	idc := Credential{AuthMethod: AuthMethodIDC, AuthRegion: "eu-central-1", RefreshToken: "rt", ClientID: "cid", ClientSecret: "sec"}
	if _, err := refreshCredential(idc, cfg, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	ext := Credential{AuthMethod: AuthMethodExternalIDP, AuthRegion: "us-east-1", RefreshToken: "rt", ClientID: "cid",
		TokenEndpoint: "https://idp.example.com/token", Scopes: "openid offline_access"}
	got, err := refreshCredential(ext, cfg, "", time.Now())
	if err != nil || got.AccessToken != "at3" {
		t.Fatalf("external idp %+v err=%v", got, err)
	}
	var idcBody map[string]string
	_ = json.Unmarshal((*seen)[0].Body, &idcBody)
	if (*seen)[0].URL != "https://oidc.eu-central-1.amazonaws.com/token" || idcBody["grantType"] != "refresh_token" || idcBody["clientSecret"] != "sec" {
		t.Fatalf("idc request %s %v", (*seen)[0].URL, idcBody)
	}
	form, _ := url.ParseQuery(string((*seen)[1].Body))
	if form.Get("grant_type") != "refresh_token" || form.Get("client_id") != "cid" || form.Get("scope") != "openid offline_access" {
		t.Fatalf("external idp form %v", form)
	}
}

func TestRefreshInvalidGrant(t *testing.T) {
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return &hostapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"error":"invalid_grant"}`)}, nil
	})
	_, err := refreshCredential(Credential{AuthMethod: AuthMethodSocial, AuthRegion: "us-east-1", RefreshToken: "rt"}, config.Get(), "", time.Now())
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("got %v", err)
	}
}

func callHandler[T any](t *testing.T, method string, req any) T {
	t.Helper()
	raw, _ := json.Marshal(req)
	out, err := HandleMethod(method, raw)
	if err != nil {
		t.Fatal(err)
	}
	var env wire.Envelope
	_ = json.Unmarshal(out, &env)
	if !env.OK {
		t.Fatalf("envelope error %+v", env.Error)
	}
	var resp T
	_ = json.Unmarshal(env.Result, &resp)
	return resp
}

const legacyCatalogField = `"kiro4cpa_models":{"fetchedAt":"2026-10-20T12:00:00Z","models":[{"modelId":"legacy-only-model"}]}`

func TestAuthRefreshOnlyRequestsTokenAndRemovesLegacyCatalog(t *testing.T) {
	for _, tc := range []struct {
		name, storage, tokenURL string
	}{
		{"social", kiroRsSocial, "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"},
		{"builder ID without profile", `{"authMethod":"builder-id","authRegion":"us-east-1","refreshToken":"rt","clientId":"cid","clientSecret":"sec","subscriptionTitle":"KIRO FREE"}`, "https://oidc.us-east-1.amazonaws.com/token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
				if r.Method != http.MethodPost || r.URL != tc.tokenURL || r.HostCallbackID != "cb-9" {
					t.Fatalf("unexpected upstream request: %s %s (callback %q)", r.Method, r.URL, r.HostCallbackID)
				}
				return ok(`{"accessToken":"at2","refreshToken":"rt2","expiresIn":3600}`)
			})
			storage := strings.Replace(tc.storage, `{`, `{`+legacyCatalogField+`,`, 1)
			req := authRefreshRPC{AuthRefreshRequest: pluginapi.AuthRefreshRequest{AuthID: "kiro.json", StorageJSON: []byte(storage),
				Metadata: map[string]any{"email": "a@b.c", "priority": 5.0}}, HostCallbackID: "cb-9"}
			resp := callHandler[pluginapi.AuthRefreshResponse](t, pluginabi.MethodAuthRefresh, req)
			if len(*seen) != 1 {
				t.Fatalf("upstream requests = %d, want only the token refresh", len(*seen))
			}
			stored, err := decodeFields(resp.Auth.StorageJSON)
			if err != nil {
				t.Fatal(err)
			}
			if stored["accessToken"] != "at2" || stored["refreshToken"] != "rt2" || stored["subscriptionTitle"] != "KIRO FREE" {
				t.Fatalf("stored fields %v", stored)
			}
			if _, ok := stored[catalogKey]; ok {
				t.Fatal("refresh retained the legacy catalog")
			}
			if resp.Auth.Metadata["email"] != "a@b.c" || resp.Auth.Metadata["priority"] != 5.0 || resp.Auth.Metadata[refreshIntervalKey] != float64(refreshIntervalSeconds) || resp.Auth.Disabled {
				t.Fatalf("host metadata or account status lost: %+v", resp.Auth)
			}
			if d := time.Until(resp.NextRefreshAfter); d < 45*time.Minute || d > 55*time.Minute {
				t.Fatalf("next refresh in %v", d)
			}
		})
	}
}

func TestAuthRefreshInvalidGrantDisables(t *testing.T) {
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return &hostapi.HTTPResponse{StatusCode: 400, Body: []byte(`invalid_grant`)}, nil
	})
	resp := callHandler[pluginapi.AuthRefreshResponse](t, pluginabi.MethodAuthRefresh,
		authRefreshRPC{AuthRefreshRequest: pluginapi.AuthRefreshRequest{StorageJSON: []byte(kiroRsSocial)}})
	if !resp.Auth.Disabled || resp.Auth.Metadata["disabled"] != true || time.Until(resp.NextRefreshAfter) < time.Hour {
		t.Fatalf("got %+v", resp)
	}
}

func TestAPIKeyCredentialSkipsRefresh(t *testing.T) {
	seen := stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) { return ok(`{}`) })
	c := Credential{AuthMethod: AuthMethodAPIKey, KiroAPIKey: "ksk_1"}
	now := time.Now()
	got, err := refreshCredential(c, config.Get(), "", now)
	if err != nil || got != c || len(*seen) != 0 {
		t.Fatalf("got %+v err=%v calls=%d", got, err, len(*seen))
	}
	if next := scheduledRefresh(c, now); !next.Equal(now.Add(longRefreshInterval)) {
		t.Fatalf("api key next refresh %v", next)
	}
}

func TestRefreshUpstreamErrorIsClassified(t *testing.T) {
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return &hostapi.HTTPResponse{StatusCode: 401, Body: []byte("nope")}, nil
	})
	_, err := refreshCredential(Credential{AuthMethod: AuthMethodSocial, AuthRegion: "us-east-1", RefreshToken: "rt"}, config.Get(), "", time.Now())
	var up *UpstreamError
	if !errors.As(err, &up) || up.Kind != ErrTokenInvalid {
		t.Fatalf("got %v", err)
	}
}

func TestRegisterAppliesPluginConfig(t *testing.T) {
	t.Cleanup(func() { _ = config.UpdateFromYAML([]byte("enabled: true\n")) })
	reg := callHandler[registration](t, pluginabi.MethodPluginRegister, map[string]any{"config_yaml": []byte("enabled: true\nclient_version: 9.9.9\n")})
	if reg.Metadata.Name != "Kiro" || !reg.Capabilities.AuthProvider || config.Get().ClientVersion != "9.9.9" {
		t.Fatalf("registration %+v client_version=%s", reg.Metadata, config.Get().ClientVersion)
	}
	// Static scope would bypass oauth-excluded-models (see plugin.go).
	if reg.Capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth {
		t.Fatalf("executor_model_scope = %q, want oauth", reg.Capabilities.ExecutorModelScope)
	}
}

func TestDispatchStaticMethods(t *testing.T) {
	for _, m := range []string{pluginabi.MethodModelRegister, pluginabi.MethodModelStatic, pluginabi.MethodAuthIdentifier,
		pluginabi.MethodExecutorIdentifier, pluginabi.MethodPluginShutdown} {
		out, err := HandleMethod(m, nil)
		var env wire.Envelope
		if err != nil || json.Unmarshal(out, &env) != nil || !env.OK {
			t.Errorf("%s: env=%s err=%v", m, out, err)
		}
	}
	out, _ := HandleMethod("no.such.method", nil)
	var env wire.Envelope
	if json.Unmarshal(out, &env) != nil || env.OK {
		t.Fatalf("unknown method accepted: %s", out)
	}
}
