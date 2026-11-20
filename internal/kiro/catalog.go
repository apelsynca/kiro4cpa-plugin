package kiro

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/config"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/models"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/wire"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The per-account model catalogue: fetched from ListAvailableModels on refresh
// and stored in the auth file, so CPA registration never waits on the network.
const (
	catalogKey    = "kiro4cpa_models"
	maxModelPages = 10
)

type authModelRPC struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleModelForAuth reads only the stored catalogue so CPA registration never waits on the network.
func handleModelForAuth(request []byte) ([]byte, error) {
	var req authModelRPC
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	infos := models.GetDefaultModelInfos()
	if fields, err := decodeFields(req.StorageJSON); err == nil {
		if catalog, ok := catalogFrom(fields); ok {
			infos = models.ToModelInfos(catalog)
		}
	}
	infos = withoutModelExclusions(infos, req.AuthModelRequest)
	return wire.OK(pluginapi.ModelResponse{Provider: models.ProviderName, Models: infos}), nil
}

func fetchModelCatalog(c Credential, cfg config.PluginConfig, callbackID string) ([]models.UpstreamModel, error) {
	headers := runtimeHeaders(c, cfg)
	regions := restRegionCandidates(c)

	type attempt struct {
		region     string
		profileArn string
	}
	var attempts []attempt
	for _, reg := range regions {
		if arn := effectiveProfileARN(c); arn != "" {
			attempts = append(attempts, attempt{region: reg, profileArn: arn})
		}
		attempts = append(attempts, attempt{region: reg, profileArn: ""})
	}

	var lastErr error
	retry500 := true
	for idx, att := range attempts {
		all, err := fetchModelCatalogPages(c, att.region, att.profileArn, headers, callbackID, &retry500)
		if err == nil {
			return all, nil
		}
		lastErr = err

		var up *UpstreamError
		if errors.As(err, &up) {
			shouldRetry := up.Status == http.StatusForbidden ||
				(up.Status == http.StatusBadRequest &&
					(strings.Contains(up.Body, "Invalid profileArn") || strings.Contains(up.Body, "Improperly formed request")))
			if shouldRetry && idx+1 < len(attempts) {
				continue
			}
		}
		break
	}
	return nil, lastErr
}

func fetchModelCatalogPages(c Credential, region, profileArn string, headers map[string][]string, callbackID string, retry500 *bool) ([]models.UpstreamModel, error) {
	var all []models.UpstreamModel
	nextToken := ""
	for page := 0; page < maxModelPages; page++ {
		req := hostapi.HTTPRequest{
			HostCallbackID: callbackID,
			Method:         http.MethodGet,
			URL:            listModelsURLWith(c, region, profileArn, nextToken),
			Headers:        headers,
		}
		resp, err := hostHTTPDo(req)
		if err == nil && resp.StatusCode == http.StatusInternalServerError && *retry500 {
			*retry500 = false
			log.Printf("kiro4cpa: retrying model listing: %v", modelCatalogHTTPError(region, profileArn != "", resp))
			resp, err = hostHTTPDo(req)
		}
		if err != nil {
			return nil, &modelCatalogError{cause: err, region: region, profileSent: profileArn != ""}
		}
		if resp.StatusCode != http.StatusOK {
			return nil, modelCatalogHTTPError(region, profileArn != "", resp)
		}
		var body struct {
			Models    []models.UpstreamModel `json:"models"`
			NextToken string                 `json:"nextToken"`
		}
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			return nil, fmt.Errorf("decode kiro models: %w", err)
		}
		all = append(all, body.Models...)
		if body.NextToken == "" {
			if len(all) == 0 {
				return nil, errors.New("kiro returned an empty model list")
			}
			return all, nil
		}
		nextToken = body.NextToken
	}
	return nil, fmt.Errorf("kiro model list exceeded %d pages", maxModelPages)
}

// Keep upstream details available for classification without logging bodies or URLs.
type modelCatalogError struct {
	cause       error
	status      int
	region      string
	profileSent bool
	requestID   string
}

func (e *modelCatalogError) Error() string {
	if e.status == 0 {
		return fmt.Sprintf("kiro model listing transport failed (region=%s profile_arn_sent=%t)", e.region, e.profileSent)
	}
	return fmt.Sprintf("kiro upstream HTTP %d (region=%s profile_arn_sent=%t request_id=%q)", e.status, e.region, e.profileSent, e.requestID)
}

func (e *modelCatalogError) Unwrap() error { return e.cause }

func modelCatalogHTTPError(region string, profileSent bool, resp *hostapi.HTTPResponse) *modelCatalogError {
	e := &modelCatalogError{
		cause: classifyUpstream(resp.StatusCode, resp.Body), status: resp.StatusCode, region: region, profileSent: profileSent,
	}
	for name, values := range resp.Headers {
		switch strings.ToLower(name) {
		case "x-amzn-requestid", "x-amzn-request-id", "x-amz-request-id":
			if len(values) != 0 && values[0] != "" {
				e.requestID = values[0][:min(len(values[0]), 128)]
				return e
			}
		}
	}
	return e
}

type storedCatalog struct {
	FetchedAt string                 `json:"fetchedAt"`
	Models    []models.UpstreamModel `json:"models"`
}

func withCatalog(fields map[string]any, catalog []models.UpstreamModel, now time.Time) map[string]any {
	out := cloneMap(fields)
	out[catalogKey] = storedCatalog{FetchedAt: now.UTC().Format(time.RFC3339), Models: catalog}
	return out
}

func catalogFrom(fields map[string]any) ([]models.UpstreamModel, bool) {
	raw, ok := fields[catalogKey]
	if !ok {
		return nil, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var stored storedCatalog
	if json.Unmarshal(b, &stored) != nil || len(stored.Models) == 0 {
		return nil, false
	}
	return stored.Models, true
}
