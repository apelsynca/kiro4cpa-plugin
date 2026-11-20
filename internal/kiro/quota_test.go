package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Shape observed from getUsageLimits for a KIRO FREE account on 2026-10-21.
const usageLimitsBody = `{"daysUntilReset":10,"nextDateReset":1793491200.0,
 "subscriptionInfo":{"subscriptionTitle":"KIRO FREE"},
 "usageBreakdownList":[{"resourceType":"CREDIT","displayName":"Credit","unit":"INVOCATIONS",
   "currentUsageWithPrecision":2.79,"usageLimitWithPrecision":50.0,"nextDateReset":1793491200.0,
   "freeTrialInfo":{"currentUsageWithPrecision":10,"usageLimitWithPrecision":40,"freeTrialExpiry":1794000000,"freeTrialStatus":"ACTIVE"},
   "bonuses":[{"displayName":"Welcome","currentUsage":0,"usageLimit":0}]}]}`

func quotaRequest() quotaFetchRPC {
	return quotaFetchRPC{QuotaFetchRequest: pluginapi.QuotaFetchRequest{AuthID: "kiro.json", Provider: "kiro4cpa", StorageJSON: []byte(kiroRsSocial)}, HostCallbackID: "cb-q"}
}

func TestQuotaFetchMapsUsageLimits(t *testing.T) {
	seen := stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) { return ok(usageLimitsBody) })
	resp := callHandler[pluginapi.QuotaFetchResponse](t, pluginabi.MethodQuotaFetch, quotaRequest())

	r := (*seen)[0]
	if r.Method != "GET" || r.HostCallbackID != "cb-q" || !strings.HasPrefix(r.URL, "https://q.us-east-1.amazonaws.com/getUsageLimits?") ||
		!strings.Contains(r.URL, "resourceType=AGENTIC_REQUEST") || !strings.Contains(r.URL, "profileArn=arn%3Aaws") || r.Headers["Authorization"][0] != "Bearer at" {
		t.Fatalf("request %+v", r)
	}
	if resp.Subscription == nil || resp.Subscription.Plan != "KIRO FREE" {
		t.Fatalf("subscription %+v", resp.Subscription)
	}
	// The summary adds the active free trial, as kiro.rs does: 2.79+10 / 50+40.
	if len(resp.Summary) != 2 || resp.Summary[0].Key != "credit_used" || resp.Summary[0].Value != 12.79 || resp.Summary[1].Value != 90 {
		t.Fatalf("summary %+v", resp.Summary)
	}
	if len(resp.Groups) != 1 || resp.Groups[0].DisplayName != "Credit" || len(resp.Groups[0].Buckets) != 2 {
		t.Fatalf("groups %+v", resp.Groups)
	}
	monthly, trial := resp.Groups[0].Buckets[0], resp.Groups[0].Buckets[1]
	if monthly.Window != "monthly" || monthly.Description != "Monthly · 2.79 / 50" || monthly.ResetTime != "2026-11-01T00:00:00Z" ||
		monthly.RemainingFraction < 0.944 || monthly.RemainingFraction > 0.945 {
		t.Fatalf("monthly %+v", monthly)
	}
	if trial.Window != "free_trial" || trial.RemainingFraction != 0.75 || trial.Description != "Free trial · 10 / 40" {
		t.Fatalf("trial %+v", trial)
	}
}

func TestQuotaFetchUpstreamError(t *testing.T) {
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return &hostapi.HTTPResponse{StatusCode: 403, Body: []byte("The bearer token included in the request is invalid")}, nil
	})
	raw, _ := json.Marshal(quotaRequest())
	out, _ := HandleMethod(pluginabi.MethodQuotaFetch, raw)
	if env := decodeEnvelope(t, out); env.OK || env.Error.HTTPStatus != 401 {
		t.Fatalf("env %s", out)
	}
}

func TestQuotaFetchRefreshesRejectedToken(t *testing.T) {
	var saved string
	origSave := hostAuthSave
	hostAuthSave = func(name string, _ []byte) error { saved = name; return nil }
	t.Cleanup(func() { hostAuthSave = origSave })
	refreshCalls := 0
	stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if strings.Contains(r.URL, "refreshToken") {
			refreshCalls++
			return ok(`{"accessToken":"fresh","expiresIn":3600}`)
		}
		if r.Headers["Authorization"][0] != "Bearer fresh" {
			return &hostapi.HTTPResponse{StatusCode: 401, Body: []byte("expired")}, nil
		}
		return ok(usageLimitsBody)
	})
	req := quotaRequest()
	req.Attributes = map[string]string{"path": "/auth/kiro.json"}
	resp := callHandler[pluginapi.QuotaFetchResponse](t, pluginabi.MethodQuotaFetch, req)
	if refreshCalls != 1 || saved != "kiro.json" || resp.Subscription == nil || resp.Subscription.Plan != "KIRO FREE" {
		t.Fatalf("refreshCalls=%d saved=%q resp=%+v", refreshCalls, saved, resp)
	}
}

func TestQuotaFetchRetriesWithoutProfileARN(t *testing.T) {
	calls := 0
	stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		calls++
		if strings.Contains(r.URL, "profileArn=") {
			return &hostapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"message":"Invalid profileArn."}`)}, nil
		}
		return ok(usageLimitsBody)
	})
	resp := callHandler[pluginapi.QuotaFetchResponse](t, pluginabi.MethodQuotaFetch, quotaRequest())
	if calls != 2 {
		t.Fatalf("expected 2 calls (retry without profileArn), got %d", calls)
	}
	if resp.Subscription == nil || resp.Subscription.Plan != "KIRO FREE" {
		t.Fatalf("subscription %+v", resp.Subscription)
	}
}

func TestQuotaFetchFallsBackToManagementForBuilderID(t *testing.T) {
	const managementURL = "https://management.us-east-1.kiro.dev/Get-Usage-Limits?isEmailRequired=false&origin=KIRO_CLI&resourceType=CREDIT"
	seen := stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if r.URL == managementURL {
			return ok(usageLimitsBody)
		}
		return &hostapi.HTTPResponse{StatusCode: 400, Body: []byte(`{"message":"Invalid profileArn."}`)}, nil
	})
	req := quotaRequest()
	req.StorageJSON = []byte(`{"authMethod":"idc","accessToken":"at","refreshToken":"rt","clientId":"client","clientSecret":"secret"}`)
	resp := callHandler[pluginapi.QuotaFetchResponse](t, pluginabi.MethodQuotaFetch, req)
	if len(*seen) != 2 || strings.Contains((*seen)[0].URL, "profileArn") {
		t.Fatalf("expected an ARN-free legacy request followed by one fallback, got %d requests", len(*seen))
	}
	r := (*seen)[1]
	if r.URL != managementURL || r.Method != "GET" || r.HostCallbackID != "cb-q" || r.Headers["Authorization"][0] != "Bearer at" {
		t.Fatalf("unexpected management quota request: %+v", r)
	}
	if resp.Subscription == nil || resp.Subscription.Plan != "KIRO FREE" || len(resp.Summary) != 2 {
		t.Fatalf("quota response %+v", resp)
	}
}

func TestQuotaDescribeAndRegistration(t *testing.T) {
	d := callHandler[pluginapi.QuotaDescribeResponse](t, pluginabi.MethodQuotaDescribe, map[string]any{})
	if len(d.SupportedProviders) != 1 || d.SupportedProviders[0] != "kiro4cpa" || d.DisplayName != "Kiro" {
		t.Fatalf("describe %+v", d)
	}
	if !getRegistration().Capabilities.QuotaProvider {
		t.Fatal("quota capability not declared")
	}
	id := callHandler[identifierResponse](t, pluginabi.MethodQuotaIdentifier, map[string]any{})
	if id.Identifier != "kiro4cpa" {
		t.Fatalf("identifier %+v", id)
	}
}

func TestQuotaIgnoresExpiredTrialAndBonus(t *testing.T) {
	// Shape returned on 2026-10-24 for a KIRO FREE account whose trial ended in April.
	var l usageLimits
	if err := json.Unmarshal([]byte(`{"subscriptionInfo":{"subscriptionTitle":"KIRO FREE"},
	 "usageBreakdownList":[{"resourceType":"CREDIT","displayName":"Credit","currentUsageWithPrecision":0,"usageLimitWithPrecision":50,
	   "freeTrialInfo":{"freeTrialStatus":"EXPIRED","currentUsageWithPrecision":0,"usageLimitWithPrecision":500,"freeTrialExpiry":1776408039.159},
	   "bonuses":[{"displayName":"Old","status":"EXPIRED","currentUsage":0,"usageLimit":100},
	              {"displayName":"No status","currentUsage":0,"usageLimit":30},
	              {"displayName":"Launch","status":"ACTIVE","currentUsage":5,"usageLimit":20}]}]}`), &l); err != nil {
		t.Fatal(err)
	}
	resp := quotaResponse(l)
	if resp.Summary[0].Value != 5 || resp.Summary[1].Value != 70 {
		t.Fatalf("summary %+v", resp.Summary)
	}
	buckets := resp.Groups[0].Buckets
	if len(buckets) != 2 || buckets[0].Window != "monthly" || buckets[1].Window != "Launch" || buckets[1].Description != "Bonus (Launch) · 5 / 20" {
		t.Fatalf("buckets %+v", buckets)
	}
}

func TestQuotaWithoutTrial(t *testing.T) {
	var l usageLimits
	_ = json.Unmarshal([]byte(`{"subscriptionInfo":{"subscriptionTitle":"KIRO PRO+"},
	 "usageBreakdownList":[{"resourceType":"CREDIT","displayName":"Credit","currentUsageWithPrecision":654.88,"usageLimitWithPrecision":2000}]}`), &l)
	resp := quotaResponse(l)
	if resp.Subscription.Plan != "KIRO PRO+" || resp.Summary[0].Value != 654.88 || resp.Summary[1].Value != 2000 || len(resp.Groups[0].Buckets) != 1 {
		t.Fatalf("resp %+v", resp)
	}
}

func TestQuotaMissingUsageIsAnError(t *testing.T) {
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return ok(`{"subscriptionInfo":{"subscriptionTitle":"KIRO PRO"}}`)
	})
	raw, _ := json.Marshal(quotaRequest())
	out, _ := handleQuotaFetch(raw)
	env := decodeEnvelope(t, out)
	if env.OK || env.Error.Code != "quota_unavailable" || env.Error.HTTPStatus != 502 {
		t.Fatalf("missing data reported as quota: %s", out)
	}
}

func TestQuotaExhaustionDescription(t *testing.T) {
	bucket := quotaBucket("monthly", "Monthly", 1000, 1000, 1796083200)
	if bucket.RemainingFraction != 0 || !strings.Contains(bucket.Description, "quota exhausted") || bucket.ResetTime != "2026-12-01T00:00:00Z" {
		t.Fatalf("bucket=%+v", bucket)
	}
}

// CPA decodes plugin quota into its SDK types before serving the panel. Keep
// labels in supported fields: unknown displayName fields on buckets are lost.
func TestQuotaLabelsSurviveHostRoundTrip(t *testing.T) {
	var limits usageLimits
	if err := json.Unmarshal([]byte(usageLimitsBody), &limits); err != nil {
		t.Fatal(err)
	}
	limits.UsageBreakdownList[0].Bonuses = []usageBonus{{DisplayName: "Welcome", Status: quotaActive, Used: 1, Limit: 10}, {Status: quotaActive, Limit: 5}}
	raw, err := json.Marshal(quotaResponse(limits))
	if err != nil {
		t.Fatal(err)
	}
	var host pluginapi.QuotaFetchResponse
	if err := json.Unmarshal(raw, &host); err != nil {
		t.Fatal(err)
	}
	if host.Summary[0].Label != "Used" || host.Summary[1].Label != "Limit" || host.Summary[0].Unit != "Credit" {
		t.Fatalf("summary labels %+v", host.Summary)
	}
	for i, want := range []string{"Monthly · 2.79 / 50", "Free trial · 10 / 40", "Bonus (Welcome) · 1 / 10", "Bonus · 0 / 5"} {
		if got := host.Groups[0].Buckets[i].Description; got != want {
			t.Errorf("bucket %d: %q, want %q", i, got, want)
		}
	}
}
