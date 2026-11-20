package kiro

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/config"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/wire"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed connect_page.html
var connectPageHTML []byte

// The connect page is a public resource and holds no data; every action goes
// through management routes, which CPA authenticates with the management key.
const (
	connectResourcePath = "/connect"
	loginRoutePrefix    = "/plugins/kiro4cpa/login/"
	routeDevice         = loginRoutePrefix + "device"
	routeSocialStart    = loginRoutePrefix + "social/start"
	routeSocialComplete = loginRoutePrefix + "social/complete"
	routeImport         = loginRoutePrefix + "import"
	routeStatus         = loginRoutePrefix + "status"
	maxPastedField      = 16 << 10
)

func connectPageURL(state string) string {
	return "/v0/resource/plugins/" + pluginID + connectResourcePath + "#state=" + state
}

type managementRegistration struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

func managementRoutes() managementRegistration {
	return managementRegistration{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodPost, Path: routeDevice},
			{Method: http.MethodPost, Path: routeSocialStart},
			{Method: http.MethodPost, Path: routeSocialComplete},
			{Method: http.MethodPost, Path: routeImport},
			{Method: http.MethodGet, Path: routeStatus},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: connectResourcePath, Description: "Adds a Kiro account; the OAuth page opens it with a login state."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(path, "/plugins/"+pluginID+connectResourcePath):
		return wire.OK(pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":            {"text/html; charset=utf-8"},
				"Cache-Control":           {"no-store"},
				"X-Content-Type-Options":  {"nosniff"},
				"Referrer-Policy":         {"no-referrer"},
				"Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"},
			},
			Body: connectPageHTML,
		}), nil
	case req.Method == http.MethodPost && strings.HasSuffix(path, routeDevice):
		return handleAPIDevice(req)
	case req.Method == http.MethodPost && strings.HasSuffix(path, routeSocialStart):
		return handleAPISocialStart(req)
	case req.Method == http.MethodPost && strings.HasSuffix(path, routeSocialComplete):
		return handleAPISocialComplete(req)
	case req.Method == http.MethodPost && strings.HasSuffix(path, routeImport):
		return handleAPIImport(req)
	case req.Method == http.MethodGet && strings.HasSuffix(path, routeStatus):
		return handleAPIStatus(req)
	default:
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// claimSession reserves a pending session for one method. A session is used
// once: picking a second method after the first started is rejected.
func claimSession(state, method string) (*LoginSession, error) {
	var claimed *LoginSession
	var conflict bool
	var busy bool
	ok := updateLoginSession(state, func(s *LoginSession) {
		if s.Completed != nil || (s.Method != "" && s.Method != method) {
			conflict = true
			return
		}
		if s.Busy || s.Polling {
			busy = true
			return
		}
		s.Method = method
		s.Error = ""
		s.Busy = true
		snapshot := *s
		claimed = &snapshot
	})
	switch {
	case !ok:
		return nil, errSessionNotFound
	case conflict:
		return nil, errSessionUsed
	case busy:
		return nil, errors.New("authorization is in progress; please wait for the result")
	}
	return claimed, nil
}

func releaseSession(state string) {
	updateLoginSession(state, func(s *LoginSession) { s.Busy = false })
}

var (
	errSessionNotFound = errors.New("sign-in session does not exist or has expired; start a new sign-in from the management panel")
	errSessionUsed     = errors.New("this session already uses another sign-in method; start a new sign-in from the management panel")
)

func sessionError(err error) ([]byte, error) {
	status := http.StatusConflict
	if errors.Is(err, errSessionNotFound) {
		status = http.StatusNotFound
	}
	return jsonResponse(status, map[string]string{"error": err.Error()})
}

func decodeBody(req pluginapi.ManagementRequest, out any) error {
	if len(req.Body) > maxPastedField*4 {
		return errors.New("request body is too large")
	}
	if err := json.Unmarshal(req.Body, out); err != nil {
		return errors.New("invalid request format")
	}
	return nil
}

type deviceRequest struct {
	State    string `json:"state"`
	Kind     string `json:"kind"` // "builder" or "idc"
	StartURL string `json:"startUrl"`
	Region   string `json:"region"`
}

func handleAPIDevice(req pluginapi.ManagementRequest) ([]byte, error) {
	var body deviceRequest
	if err := decodeBody(req, &body); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	region := firstNonEmpty(body.Region, defaultSSORegion)
	startURL := builderStartURL
	switch body.Kind {
	case "builder":
		region = defaultSSORegion
	case "idc":
		if err := validateRegion(region); err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "Invalid region format"})
		}
		if !validIdCStartURL(body.StartURL) {
			return jsonResponse(http.StatusBadRequest, map[string]string{"error": "Start URL must be https://<directory>.awsapps.com/start"})
		}
		startURL = strings.TrimRight(strings.TrimSpace(body.StartURL), "/")
	default:
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "Unsupported sign-in method"})
	}
	if _, err := claimSession(body.State, "device-"+body.Kind); err != nil {
		return sessionError(err)
	}
	defer releaseSession(body.State)

	oidc := newSSOOIDCClientFn()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	regResp, err := oidc.RegisterClient(ctx, region)
	if err != nil {
		failLoginSession(body.State, "AWS client registration failed: "+err.Error())
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "AWS client registration failed: " + err.Error()})
	}
	authResp, err := oidc.StartDeviceAuth(ctx, regResp.ClientID, regResp.ClientSecret, startURL, region)
	if err != nil {
		failLoginSession(body.State, "AWS device authorization failed: "+err.Error())
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "AWS device authorization failed: " + err.Error()})
	}
	verifyURL := firstNonEmpty(authResp.VerificationURIComplete, authResp.VerificationURI)
	if !trustedAWSURL(verifyURL) {
		failLoginSession(body.State, "AWS returned an untrusted authorization URL")
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": "AWS returned an untrusted authorization URL"})
	}

	updateLoginSession(body.State, func(s *LoginSession) {
		s.ClientID = regResp.ClientID
		s.ClientSecret = regResp.ClientSecret
		s.DeviceCode = authResp.DeviceCode
		s.UserCode = authResp.UserCode
		s.VerifyURL = verifyURL
		s.Region = region
		s.StartURL = startURL
		s.Polling = true
		expiresIn := authResp.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 600
		}
		s.DeviceExpiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
		if s.DeviceExpiresAt.After(s.ExpiresAt) {
			s.DeviceExpiresAt = s.ExpiresAt
		}
	})
	go pollDeviceToken(body.State, oidc, deviceGrant{
		clientID: regResp.ClientID, clientSecret: regResp.ClientSecret, deviceCode: authResp.DeviceCode,
		region: region, startURL: startURL, interval: authResp.Interval, expiresIn: authResp.ExpiresIn,
	})

	return jsonResponse(http.StatusOK, map[string]any{
		"userCode":        authResp.UserCode,
		"verificationUri": verifyURL,
		"expiresIn":       authResp.ExpiresIn,
	})
}

func validIdCStartURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	sub, ok := strings.CutSuffix(host, ".awsapps.com")
	return ok && sub != "" && !strings.Contains(sub, ".") && u.Port() == "" &&
		strings.TrimRight(u.Path, "/") == "/start"
}

func trustedAWSURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".awsapps.com")
}

type deviceGrant struct {
	clientID, clientSecret, deviceCode, region, startURL string
	interval, expiresIn                                  int
}

// pollDeviceInterval is shortened in tests.
var pollDeviceInterval = func(seconds int) time.Duration {
	return time.Duration(max(seconds, 5)) * time.Second
}

func pollDeviceToken(state string, oidc *SSOOIDCClient, g deviceGrant) {
	expiresIn := g.expiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	wait := pollDeviceInterval(g.interval)
	for {
		time.Sleep(wait)
		if _, ok := getLoginSession(state); !ok {
			return
		}
		if time.Now().After(deadline) {
			failLoginSession(state, "AWS authorization expired")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		tok, err := oidc.CreateToken(ctx, g.clientID, g.clientSecret, g.deviceCode, g.region)
		cancel()
		switch {
		case err == nil:
			cred := &Credential{
				AuthMethod:   AuthMethodIDC,
				Provider:     providerFor(Credential{AuthMethod: AuthMethodIDC, StartURL: g.startURL}),
				AccessToken:  tok.AccessToken,
				RefreshToken: tok.RefreshToken,
				ClientID:     g.clientID,
				ClientSecret: g.clientSecret,
				Region:       g.region,
				AuthRegion:   g.region,
				StartURL:     g.startURL,
				ProfileArn:   tok.ProfileArn,
				ExpiresAt:    time.Now().UTC().Add(time.Duration(tok.ExpiresIn) * time.Second).Format(time.RFC3339),
			}
			completeLoginSession(state, cred, "Kiro device authorization completed")
			return
		case errors.Is(err, ErrSlowDown):
			wait += 5 * time.Second
		case errors.Is(err, ErrAuthPending):
		default:
			failLoginSession(state, "AWS authorization failed: "+err.Error())
			return
		}
	}
}

type stateRequest struct {
	State string `json:"state"`
}

func handleAPISocialStart(req pluginapi.ManagementRequest) ([]byte, error) {
	var body stateRequest
	if err := decodeBody(req, &body); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	session, err := claimSession(body.State, "social")
	if err != nil {
		return sessionError(err)
	}
	defer releaseSession(body.State)
	// Reloads or a lost response must not invalidate an already-open PKCE link.
	if session.CodeVerifier != "" && session.SocialState != "" {
		return jsonResponse(http.StatusOK, map[string]string{
			"url":         socialPortalURL(session.SocialState, pkceChallenge(session.CodeVerifier)),
			"redirectUri": socialRedirectURI,
		})
	}
	verifier, err1 := randomURLToken(32)
	socialState, err2 := randomURLToken(24)
	if err1 != nil || err2 != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "Could not generate sign-in parameters"})
	}
	updateLoginSession(body.State, func(s *LoginSession) {
		s.CodeVerifier = verifier
		s.SocialState = socialState
	})
	return jsonResponse(http.StatusOK, map[string]string{
		"url":         socialPortalURL(socialState, pkceChallenge(verifier)),
		"redirectUri": socialRedirectURI,
	})
}

type socialCompleteRequest struct {
	State       string `json:"state"`
	CallbackURL string `json:"callbackUrl"`
}

func handleAPISocialComplete(req pluginapi.ManagementRequest) ([]byte, error) {
	var body socialCompleteRequest
	if err := decodeBody(req, &body); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	session, err := claimSession(body.State, "social")
	if err != nil {
		return sessionError(err)
	}
	defer releaseSession(body.State)
	if session.CodeVerifier == "" {
		return jsonResponse(http.StatusConflict, map[string]string{"error": `Select "Open Kiro sign in" first`})
	}
	cb, err := parseSocialCallback(body.CallbackURL)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if cb.State != session.SocialState {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "The callback URL's state does not match this sign-in session. Reopen the Kiro sign-in page."})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := exchangeSocialCode(ctx, cb, session.CodeVerifier, config.Get())
	if err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
	lifetime := defaultTokenLifetime
	if tok.ExpiresIn > 0 {
		lifetime = time.Duration(tok.ExpiresIn) * time.Second
	}
	cred := &Credential{
		AuthMethod:   AuthMethodSocial,
		Provider:     cb.provider(),
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ProfileArn:   tok.ProfileArn,
		Region:       defaultRegion,
		AuthRegion:   defaultRegion,
		ExpiresAt:    time.Now().UTC().Add(lifetime).Format(time.RFC3339),
	}
	completeLoginSession(body.State, cred, "Kiro sign-in with "+methodLabel(*cred)+" completed")
	return jsonResponse(http.StatusOK, map[string]bool{"ok": true})
}

type importRequest struct {
	State        string `json:"state"`
	Method       string `json:"method"` // "social", "idc" or "api_key"
	RefreshToken string `json:"refreshToken"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Region       string `json:"region"`
	KiroAPIKey   string `json:"kiroApiKey"`
}

func handleAPIImport(req pluginapi.ManagementRequest) ([]byte, error) {
	var body importRequest
	if err := decodeBody(req, &body); err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	cred, err := importedCredential(body)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	if _, err := claimSession(body.State, "import-"+cred.AuthMethod); err != nil {
		return sessionError(err)
	}
	defer releaseSession(body.State)
	validation, warning := "valid", ""
	if cred.AuthMethod == AuthMethodAPIKey {
		_, err = fetchUsageLimits(cred, config.Get(), "")
	} else {
		cred, err = refreshCredential(cred, config.Get(), "", time.Now())
	}
	if err != nil {
		validation, warning = "unverified", "Credentials could not be verified right now. Check the account status in the management panel."
		var up *UpstreamError
		if errors.Is(err, ErrInvalidGrant) || (errors.As(err, &up) && up.Kind == ErrTokenInvalid) {
			validation, warning = "invalid", "Credentials are no longer valid. Reauthorize or import valid credentials."
		}
	}
	updateLoginSession(body.State, func(s *LoginSession) {
		s.Validation, s.Warning = validation, warning
	})
	completeLoginSession(body.State, &cred, "Kiro credentials imported")
	return handleAPIStatus(pluginapi.ManagementRequest{Query: url.Values{"state": {body.State}}})
}

func importedCredential(body importRequest) (Credential, error) {
	trim := func(s string) string { return strings.TrimSpace(s) }
	for _, v := range []string{body.RefreshToken, body.ClientID, body.ClientSecret, body.KiroAPIKey} {
		if len(v) > maxPastedField {
			return Credential{}, errors.New("credential data is too long")
		}
	}
	region := firstNonEmpty(body.Region, defaultRegion)
	var cred Credential
	switch body.Method {
	case "api_key":
		cred = Credential{AuthMethod: AuthMethodAPIKey, KiroAPIKey: trim(body.KiroAPIKey)}
	case "social":
		cred = Credential{AuthMethod: AuthMethodSocial, RefreshToken: trim(body.RefreshToken), Region: region, AuthRegion: region}
	case "idc":
		cred = Credential{AuthMethod: AuthMethodIDC, RefreshToken: trim(body.RefreshToken),
			ClientID: trim(body.ClientID), ClientSecret: trim(body.ClientSecret), Region: region, AuthRegion: region}
	default:
		return Credential{}, errors.New("unsupported import method")
	}
	cred = cred.normalized()
	if err := cred.validate(); err != nil {
		return Credential{}, err
	}
	cred.Provider = providerFor(cred)
	return cred, nil
}

func handleAPIStatus(req pluginapi.ManagementRequest) ([]byte, error) {
	session, ok := getLoginSession(req.Query.Get("state"))
	if !ok {
		return jsonResponse(http.StatusNotFound, map[string]string{"status": "error", "error": errSessionNotFound.Error()})
	}
	data := map[string]any{
		"status": "pending", "method": session.Method, "expiresAt": session.ExpiresAt,
		"busy": session.Busy || session.Polling, "validation": session.Validation,
		"warning": session.Warning, "delivered": session.Delivered,
	}
	// Only public authorization details are exposed, never the device secret,
	// code verifier, callback code or tokens.
	if session.VerifyURL != "" {
		data["verificationUri"], data["userCode"] = session.VerifyURL, session.UserCode
		data["deviceExpiresAt"] = session.DeviceExpiresAt
		data["startUrl"], data["region"] = session.StartURL, session.Region
	}
	if session.SocialState != "" && session.CodeVerifier != "" {
		data["url"] = socialPortalURL(session.SocialState, pkceChallenge(session.CodeVerifier))
	}
	switch {
	case session.Completed != nil:
		data["status"], data["message"] = "success", session.Message
	case session.Error != "":
		data["status"], data["error"] = "error", session.Error
	}
	return jsonResponse(http.StatusOK, data)
}

func jsonResponse(statusCode int, data any) ([]byte, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return wire.OK(pluginapi.ManagementResponse{
		StatusCode: statusCode,
		Headers: http.Header{
			"Content-Type":           {"application/json; charset=utf-8"},
			"Cache-Control":          {"no-store"},
			"X-Content-Type-Options": {"nosniff"},
		},
		Body: b,
	}), nil
}
