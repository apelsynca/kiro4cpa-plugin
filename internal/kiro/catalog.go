package kiro

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
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
	headers := runtimeHeadersWithVersion(c, cfg, cfg.ClientVersion)
	regions := restRegionCandidates(c)
	profileARN := effectiveProfileARN(c)
	if profileARN == "" && (c.AuthMethod == AuthMethodIDC || c.AuthMethod == AuthMethodExternalIDP) {
		if arn, region := resolveCatalogProfile(regions, headers, callbackID); arn != "" {
			profileARN = arn
			if region != regions[0] {
				regions[0], regions[1] = regions[1], regions[0]
			}
		}
	}

	type attempt struct {
		region     string
		profileArn string
	}
	var attempts []attempt
	for _, reg := range regions {
		if profileARN != "" {
			attempts = append(attempts, attempt{region: reg, profileArn: profileARN})
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

func catalogRequestHeaders(headers map[string][]string) map[string][]string {
	h := maps.Clone(headers)
	h["amz-sdk-invocation-id"] = []string{newUUID()}
	h["amz-sdk-request"] = []string{"attempt=1; max=1"}
	return h
}

func resolveCatalogProfile(regions []string, headers map[string][]string, callbackID string) (string, string) {
	for _, region := range regions {
		h := catalogRequestHeaders(headers)
		h["Content-Type"] = []string{"application/x-amz-json-1.0"}
		h["x-amz-target"] = []string{"AmazonCodeWhispererService.ListAvailableProfiles"}
		resp, err := hostHTTPDo(hostapi.HTTPRequest{
			HostCallbackID: callbackID,
			Method:         http.MethodPost,
			URL:            fmt.Sprintf("https://q.%s.amazonaws.com/", region),
			Headers:        h,
			Body:           []byte(`{"maxResults":10}`),
		})
		if err != nil || resp.StatusCode == http.StatusTooManyRequests {
			break
		}
		if resp.StatusCode == http.StatusForbidden && strings.Contains(string(resp.Body), "AWS Builder ID is not supported") {
			break
		}
		if resp.StatusCode != http.StatusOK {
			continue
		}
		var body struct {
			Profiles []struct {
				ARN string `json:"arn"`
			} `json:"profiles"`
		}
		if json.Unmarshal(resp.Body, &body) == nil {
			for _, profile := range body.Profiles {
				if arn := strings.TrimSpace(profile.ARN); arn != "" && arn != builderIDPlaceholderARN {
					return arn, region
				}
			}
		}
	}
	return "", ""
}

func fetchModelCatalogPages(c Credential, region, profileArn string, headers map[string][]string, callbackID string, retry500 *bool) ([]models.UpstreamModel, error) {
	var all []models.UpstreamModel
	nextToken := ""
	for page := 0; page < maxModelPages; page++ {
		req := hostapi.HTTPRequest{
			HostCallbackID: callbackID,
			Method:         http.MethodGet,
			URL:            listModelsURLWith(c, region, profileArn, nextToken),
			Headers:        catalogRequestHeaders(headers),
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
