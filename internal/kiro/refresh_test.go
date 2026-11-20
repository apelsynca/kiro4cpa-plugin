package kiro

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/config"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/models"
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
	if ua := r.Headers["User-Agent"][0]; ua != "KiroIDE-1.4.5-0b57ce32592129d9e832f82147ef2a6cea350b58ca1e0596cfa331778daf69ff" {
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

func TestAuthRefreshStoresTokenAndCatalog(t *testing.T) {
	listCalls := 0
	seen := stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if strings.Contains(r.URL, "ListAvailableModels") {
			listCalls++
			if listCalls == 1 {
				return &hostapi.HTTPResponse{StatusCode: http.StatusInternalServerError}, nil
			}
			return ok(`{"models":[{"modelId":"claude-haiku-4.5","tokenLimits":{"maxInputTokens":200000}},{"modelId":"glm-5"}]}`)
		}
		return ok(`{"accessToken":"at2","expiresIn":3600}`)
	})
	req := authRefreshRPC{AuthRefreshRequest: pluginapi.AuthRefreshRequest{AuthID: "kiro.json", StorageJSON: []byte(kiroRsSocial),
		Metadata: map[string]any{"email": "a@b.c", "priority": 5.0}}, HostCallbackID: "cb-9"}
	resp := callHandler[pluginapi.AuthRefreshResponse](t, pluginabi.MethodAuthRefresh, req)

	var stored map[string]any
	_ = json.Unmarshal(resp.Auth.StorageJSON, &stored)
	if stored["accessToken"] != "at2" || stored["subscriptionTitle"] != "KIRO FREE" || stored[catalogKey] == nil {
		t.Fatalf("stored %v", stored)
	}
	if resp.Auth.Metadata["priority"] != 5.0 || resp.Auth.Metadata[refreshIntervalKey] != float64(refreshIntervalSeconds) || resp.Auth.Disabled {
		t.Fatalf("host metadata lost: %+v", resp.Auth)
	}
	if d := time.Until(resp.NextRefreshAfter); d < 45*time.Minute || d > 55*time.Minute {
		t.Fatalf("next refresh in %v", d)
	}
	list := (*seen)[1]
	if list.HostCallbackID != "cb-9" || !strings.Contains(list.URL, "profileArn=arn%3Aaws%3Acodewhisperer%3Aus-east-1%3A111%3Aprofile%2FX") {
		t.Fatalf("list request %+v", list)
	}

	forAuth := callHandler[pluginapi.ModelResponse](t, pluginabi.MethodModelForAuth,
		authModelRPC{AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: resp.Auth.StorageJSON}})
	ids := map[string]bool{}
	for _, m := range forAuth.Models {
		ids[m.ID] = true
	}
	if len(ids) != 2 || !ids["claude-haiku-4.5"] || !ids["glm-5"] {
		t.Fatalf("model ids %v", ids)
	}
}

func TestAuthRefreshKeepsCatalogWhenListingFails(t *testing.T) {
	seen := stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if strings.Contains(r.URL, "ListAvailableModels") {
			return &hostapi.HTTPResponse{StatusCode: http.StatusInternalServerError}, nil
		}
		return ok(`{"accessToken":"at2","expiresIn":3600}`)
	})
	withOld := strings.Replace(kiroRsSocial, `"id":1,`, `"id":1,"kiro4cpa_models":{"fetchedAt":"x","models":[{"modelId":"auto"}]},`, 1)
	resp := callHandler[pluginapi.AuthRefreshResponse](t, pluginabi.MethodAuthRefresh,
		authRefreshRPC{AuthRefreshRequest: pluginapi.AuthRefreshRequest{StorageJSON: []byte(withOld)}})
	fields, _ := decodeFields(resp.Auth.StorageJSON)
	if catalog, ok := catalogFrom(fields); !ok || len(catalog) != 1 || catalog[0].ModelID != "auto" {
		t.Fatalf("catalog %v", catalog)
	}
	if resp.Auth.Disabled || fields["accessToken"] != "at2" || fields[catalogKey].(map[string]any)["fetchedAt"] != "x" {
		t.Fatal("a listing failure should preserve the catalog timestamp and still refresh credentials")
	}
	if len(*seen) != 3 || !reflect.DeepEqual((*seen)[1], (*seen)[2]) {
		t.Fatalf("expected one retry of the same listing request, got %d total requests", len(*seen))
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

func TestModelForAuthFallsBackWithoutCatalog(t *testing.T) {
	resp := callHandler[pluginapi.ModelResponse](t, pluginabi.MethodModelForAuth,
		authModelRPC{AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: []byte(kiroRsSocial)}})
	if len(resp.Models) != len(models.FallbackModels) || resp.Models[0].ID != "auto" {
		t.Fatalf("want the %d fallback models, got %d", len(models.FallbackModels), len(resp.Models))
	}
}

func TestFetchModelCatalogPaginates(t *testing.T) {
	seen := stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if strings.Contains(r.URL, "nextToken=p2") {
			return ok(`{"models":[{"modelId":"glm-5"}]}`)
		}
		return ok(`{"models":[{"modelId":"auto"}],"nextToken":"p2"}`)
	})
	got, err := fetchModelCatalog(Credential{AuthMethod: AuthMethodSocial, AuthRegion: "us-east-1", AccessToken: "at", RefreshToken: "rt"}, config.Get(), "")
	if err != nil || len(got) != 2 || got[1].ModelID != "glm-5" || len(*seen) != 2 {
		t.Fatalf("models=%v err=%v calls=%d", got, err, len(*seen))
	}
}

func TestFetchModelCatalogResolvesMissingIDCProfile(t *testing.T) {
	const arn = "arn:aws:codewhisperer:eu-central-1:123456789012:profile/REAL"
	seen := stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if r.Method == http.MethodPost {
			if r.URL == "https://q.us-east-1.amazonaws.com/" {
				return ok(`{"profiles":[]}`)
			}
			return ok(`{"profiles":[{"arn":"` + arn + `"}]}`)
		}
		return ok(`{"models":[{"modelId":"live-model"}]}`)
	})
	got, err := fetchModelCatalog(Credential{AuthMethod: AuthMethodIDC, AccessToken: "at", ProfileArn: builderIDPlaceholderARN}, config.Get(), "cb-1")
	if err != nil || len(got) != 1 || got[0].ModelID != "live-model" || len(*seen) != 3 {
		t.Fatalf("models=%v err=%v calls=%d", got, err, len(*seen))
	}
	for _, req := range (*seen)[:2] {
		if req.Method != http.MethodPost || req.Headers["x-amz-target"][0] != "AmazonCodeWhispererService.ListAvailableProfiles" ||
			req.Headers["Content-Type"][0] != "application/x-amz-json-1.0" || string(req.Body) != `{"maxResults":10}` {
			t.Fatalf("unexpected profile request: %+v", req)
		}
	}
	list := (*seen)[2]
	u, err := url.Parse(list.URL)
	if err != nil || list.Method != http.MethodGet || u.Host != "q.eu-central-1.amazonaws.com" || u.Query().Get("profileArn") != arn {
		t.Fatalf("unexpected model list request: %+v", list)
	}
	ids := map[string]bool{}
	for _, req := range *seen {
		h := http.Header(req.Headers)
		id := firstNonEmpty(req.Headers["amz-sdk-invocation-id"]...)
		if id == "" || ids[id] || firstNonEmpty(req.Headers["amz-sdk-request"]...) != "attempt=1; max=1" ||
			h.Get("Authorization") != "Bearer at" || req.HostCallbackID != "cb-1" {
			t.Fatalf("unexpected SDK headers: %+v", req)
		}
		ids[id] = true
	}
}

func TestFetchModelCatalogBuilderIDFallsBackWithoutProfile(t *testing.T) {
	seen := stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if r.Method == http.MethodPost {
			return &hostapi.HTTPResponse{StatusCode: http.StatusForbidden, Body: []byte(`{"message":"AWS Builder ID is not supported"}`)}, nil
		}
		return ok(`{"models":[{"modelId":"auto"}]}`)
	})
	got, err := fetchModelCatalog(Credential{AuthMethod: AuthMethodIDC, AccessToken: "at"}, config.Get(), "cb-1")
	if err != nil || len(got) != 1 || len(*seen) != 2 || (*seen)[1].URL != "https://q.us-east-1.amazonaws.com/ListAvailableModels?origin=AI_EDITOR" {
		t.Fatalf("models=%v err=%v calls=%d", got, err, len(*seen))
	}
}

func TestFetchModelCatalogRetriesFailedPage(t *testing.T) {
	pageTwoCalls := 0
	seen := stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if !strings.Contains(r.URL, "nextToken=p2") {
			return ok(`{"models":[{"modelId":"auto"}],"nextToken":"p2"}`)
		}
		pageTwoCalls++
		if pageTwoCalls == 1 {
			return &hostapi.HTTPResponse{StatusCode: http.StatusInternalServerError}, nil
		}
		return ok(`{"models":[{"modelId":"glm-5"}]}`)
	})
	got, err := fetchModelCatalog(Credential{AccessToken: "at", ProfileArn: "arn:test"}, config.Get(), "cb-1")
	if err != nil || len(got) != 2 || got[0].ModelID != "auto" || got[1].ModelID != "glm-5" {
		t.Fatalf("models=%v err=%v", got, err)
	}
	if len(*seen) != 3 || !reflect.DeepEqual((*seen)[1], (*seen)[2]) {
		t.Fatalf("expected only the second page to be retried, got %d requests", len(*seen))
	}
}

func TestFetchModelCatalogSharesRetryBudgetAcrossPages(t *testing.T) {
	calls := 0
	seen := stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		calls++
		if calls == 2 {
			return ok(`{"models":[{"modelId":"auto"}],"nextToken":"p2"}`)
		}
		return &hostapi.HTTPResponse{
			StatusCode: http.StatusInternalServerError,
			Headers:    map[string][]string{"X-Amzn-RequestId": {"trace-id"}},
			Body:       []byte("sensitive body"),
		}, nil
	})
	got, err := fetchModelCatalog(Credential{AccessToken: "at", ProfileArn: "arn:test"}, config.Get(), "cb-1")
	var up *UpstreamError
	if len(got) != 0 || len(*seen) != 3 || !errors.As(err, &up) || up.Status != http.StatusInternalServerError {
		t.Fatalf("models=%v err=%v calls=%d", got, err, len(*seen))
	}
	if want := `kiro upstream HTTP 500 (region=us-east-1 profile_arn_sent=true request_id="trace-id")`; err.Error() != want {
		t.Fatalf("diagnostic = %q, want %q", err, want)
	}
}

func TestFetchModelCatalogRetriesWithoutProfileARN(t *testing.T) {
	seen := stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if strings.Contains(r.URL, "profileArn=") {
			return &hostapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"message":"Invalid profileArn."}`)}, nil
		}
		return ok(`{"models":[{"modelId":"auto"}]}`)
	})
	got, err := fetchModelCatalog(Credential{AuthMethod: AuthMethodSocial, AuthRegion: "us-east-1", AccessToken: "at", RefreshToken: "rt", ProfileArn: "arn:broken"}, config.Get(), "")
	if err != nil || len(got) != 1 || got[0].ModelID != "auto" || len(*seen) != 2 {
		t.Fatalf("models=%v err=%v calls=%d", got, err, len(*seen))
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
