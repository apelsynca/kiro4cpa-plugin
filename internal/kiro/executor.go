package kiro

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/config"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/wire"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type executorRPC struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// Host callbacks are swapped in tests.
var (
	hostAuthSave      = hostapi.AuthSave
	hostHTTPDoStream  = hostapi.HTTPDoStream
	hostStreamRead    = hostapi.HTTPStreamRead
	hostHTTPStreamEnd = hostapi.HTTPStreamClose
	hostStreamEmit    = hostapi.StreamEmit
	hostStreamClose   = hostapi.StreamClose
)

// prepareExecution decodes the credential and the Claude payload; a non-nil
// envelope is the error answer to return as-is.
func prepareExecution(request []byte) (executorRPC, Credential, convertedRequest, []byte, error) {
	var req executorRPC
	if err := json.Unmarshal(request, &req); err != nil {
		return req, Credential{}, convertedRequest{}, nil, err
	}
	cred, _, err := decodeCredential(req.StorageJSON)
	if err != nil {
		return req, cred, convertedRequest{}, wire.ErrorStatus("invalid_credential", err.Error(), http.StatusUnauthorized, false), nil
	}
	conv, err := convertClaudeRequest(req.Payload)
	if err != nil {
		return req, cred, conv, wire.ErrorStatus("invalid_request", err.Error(), http.StatusBadRequest, false), nil
	}
	return req, cred, conv, nil, nil
}

func handleExecute(request []byte) ([]byte, error) {
	req, cred, conv, errEnv, err := prepareExecution(request)
	if err != nil || errEnv != nil {
		return errEnv, err
	}
	resp, err := withRefresh(req.refreshTarget(), cred, func(c Credential) (*hostapi.HTTPResponse, error) {
		return postGenerate(req.AuthID, req.HostCallbackID, c, conv, config.Get())
	}, conv.dropRejectedReasoning)
	if err != nil {
		return upstreamErrorEnvelope(err), nil
	}
	var collector responseCollector
	var dec Decoder
	for _, f := range dec.Feed(resp.Body) {
		ev, errEv := DecodeEvent(f)
		if errEv != nil {
			return wire.ErrorStatus("upstream_stream_error", errEv.Error(), http.StatusBadGateway, true), nil
		}
		collector.add(ev)
	}
	if err := dec.Finish(); err != nil {
		return wire.ErrorStatus("upstream_stream_error", err.Error(), http.StatusBadGateway, true), nil
	}
	// Any in-band error means the answer is incomplete; partial content must not
	// be returned as a finished message.
	if validationErr := collector.validate(); validationErr != nil {
		return wire.ErrorStatus("upstream_stream_error", validationErr.Error(), http.StatusBadGateway, validationErr.Code != "ContentFiltered"), nil
	}
	return wire.OK(pluginapi.ExecutorResponse{Payload: collector.message(conv)}), nil
}

func handleCountTokens(request []byte) ([]byte, error) {
	var req executorRPC
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	var claude claudeRequest
	if err := json.Unmarshal(req.Payload, &claude); err != nil {
		return wire.ErrorStatus("invalid_request", err.Error(), http.StatusBadRequest, false), nil
	}
	payload, _ := json.Marshal(map[string]int{"input_tokens": estimateRequestTokens(claude)})
	return wire.OK(pluginapi.ExecutorResponse{Payload: payload}), nil
}

// refreshTarget is what a request carries for refreshing its credential and
// writing the result back to the auth file.
type refreshTarget struct {
	authID      string
	path        string
	callbackID  string
	storageJSON []byte
}

func (r executorRPC) refreshTarget() refreshTarget {
	return refreshTarget{authID: r.AuthID, path: r.AuthAttributes["path"], callbackID: r.HostCallbackID, storageJSON: r.StorageJSON}
}

// withRefresh runs send and, when the bearer token is rejected, forces one refresh
// and retries. CPA only cools a 401'd credential down, so without this a
// single-account setup would be unusable until the next scheduled refresh.
// Concurrent rejections of one credential share a single refresh.
func withRefresh[T any](target refreshTarget, cred Credential, send func(Credential) (T, error), recoverRequest ...func(error) bool) (T, error) {
	// A failed disk write can leave CPA holding either the old or the refreshed
	// credential. Repair either snapshot even when its access token still works.
	if hasPendingRefreshSave(target.authID, cred) {
		var err error
		cred, err = refreshForRequest(target, cred, true)
		if err != nil {
			var zero T
			return zero, err
		}
	}
	resp, err := send(cred)
	// Protocol recovery and token recovery share the existing one-retry budget.
	if err != nil && len(recoverRequest) > 0 && recoverRequest[0](err) {
		return send(cred)
	}
	var up *UpstreamError
	if !errors.As(err, &up) || up.Kind != ErrTokenInvalid || cred.AuthMethod == AuthMethodAPIKey {
		return resp, err
	}
	refreshed, errRefresh := refreshForRequest(target, cred, false)
	if errRefresh != nil {
		if errors.Is(errRefresh, ErrInvalidGrant) {
			return resp, &UpstreamError{Status: http.StatusUnauthorized, Kind: ErrTokenInvalid, Body: "Kiro sign-in has expired. Please reauthorize your account."}
		}
		return resp, errRefresh
	}
	return send(refreshed)
}

func refreshForRequest(target refreshTarget, cred Credential, repairSave bool) (Credential, error) {
	return coordinateRefresh(target.authID, cred, func(c Credential) (Credential, error) {
		return refreshCredential(c, config.Get(), target.callbackID, time.Now())
	}, func(c Credential) error {
		return persistCredential(target, c)
	}, repairSave)
}

func generateRequest(callbackID string, cred Credential, conv convertedRequest, cfg config.PluginConfig) (hostapi.HTTPRequest, error) {
	url, err := generateURL(cred, cfg)
	if err != nil {
		return hostapi.HTTPRequest{}, err
	}
	payload := conv.Payload
	payload.ProfileArn = streamingProfileARN(cred)
	body, err := json.Marshal(payload)
	if err != nil {
		return hostapi.HTTPRequest{}, err
	}
	return hostapi.HTTPRequest{
		HostCallbackID: callbackID, Method: http.MethodPost, URL: url,
		Headers: streamHeaders(cred, cfg), Body: body,
	}, nil
}

// postGenerate paces and parks per credential. It sits below withRefresh so the
// retry after a token refresh is paced as its own request.
func postGenerate(authID, callbackID string, cred Credential, conv convertedRequest, cfg config.PluginConfig) (*hostapi.HTTPResponse, error) {
	if err := admitCredential(authID, cred, cfg); err != nil {
		return nil, err
	}
	httpReq, err := generateRequest(callbackID, cred, conv, cfg)
	if err != nil {
		return nil, err
	}
	resp, err := hostHTTPDo(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kiro request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		upErr := classifyUpstream(resp.StatusCode, resp.Body)
		recordUpstreamFailure(authID, upErr, cfg)
		return nil, upErr
	}
	return resp, nil
}

// persistCredential writes a refreshed token back so a rotated refresh token is
// not lost. The refresh coordinator retains failed saves for the next caller.
func persistCredential(target refreshTarget, cred Credential) error {
	name := filepath.Base(target.path)
	if name == "." || name == "/" {
		name = filepath.Base(target.authID)
	}
	if name == "." || name == "/" || name == "" {
		return errors.New("cannot persist refreshed credential without an auth file name")
	}
	fields, err := decodeFields(target.storageJSON)
	if err == nil {
		var raw []byte
		if raw, err = mergeCredential(fields, cred); err == nil {
			err = hostAuthSave(name, raw)
		}
	}
	if err != nil {
		log.Printf("kiro4cpa: persisting refreshed token for %s failed: %v", target.authID, err)
	}
	return err
}

func upstreamErrorEnvelope(err error) []byte {
	var up *UpstreamError
	if !errors.As(err, &up) {
		return wire.ErrorStatus("upstream_unavailable", err.Error(), http.StatusBadGateway, true)
	}
	status, retryable := map[ErrorKind]int{
		ErrTokenInvalid: http.StatusUnauthorized, ErrQuotaExhausted: http.StatusPaymentRequired,
		ErrForbidden: http.StatusForbidden, ErrRateLimited: http.StatusTooManyRequests,
		ErrBadRequest: http.StatusBadRequest,
	}[up.Kind], false
	if status == 0 {
		status, retryable = http.StatusBadGateway, true
	}
	return wire.ErrorStatus("upstream_error", up.Error(), status, retryable)
}
