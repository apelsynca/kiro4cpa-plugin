package kiro

import (
	"encoding/json"
	"slices"
	"sort"
	"testing"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/AkkunYo/kiro4cpa-plugin/internal/models"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func modelIDs(infos []pluginapi.ModelInfo, omit ...string) []string {
	ids := make([]string, 0, len(infos))
	for _, m := range infos {
		if !slices.Contains(omit, m.ID) {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

const freeModelStorage = `{"subscriptionTitle":"KIRO FREE"}`
const proModelStorage = `{"subscriptionTitle":"KIRO PRO"}`

func TestModelForAuthUsesStaticCatalog(t *testing.T) {
	for _, tc := range []struct {
		name, storage string
		want          []pluginapi.ModelInfo
	}{
		{"default", `{` + legacyCatalogField + `}`, models.GetDefaultModelInfos()},
		{"free", freeModelStorage, models.GetModelInfosForPlan("KIRO FREE")},
		{"pro with legacy catalog", `{"subscriptionTitle":"KIRO PRO",` + legacyCatalogField + `}`, models.GetModelInfosForPlan("KIRO PRO")},
		{"student", `{"subscriptionTitle":"KIRO STUDENT"}`, models.GetModelInfosForPlan("KIRO STUDENT")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubHTTP(t, func(req hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
				t.Fatalf("model registration made an upstream request: %s %s", req.Method, req.URL)
				return nil, nil
			})
			assertAccountModels(t, pluginapi.AuthModelRequest{AuthID: tc.name, StorageJSON: []byte(tc.storage)}, modelIDs(tc.want))
		})
	}
}

func TestAccountExclusionMatchesPanelAlias(t *testing.T) {
	host := pluginapi.HostConfigSummary{OAuthModelAlias: map[string][]pluginapi.ModelAlias{
		models.ProviderName: {{Name: "claude-sonnet-4.5", Alias: "claude-sonnet-4-5"}},
	}}
	pro := models.GetModelInfosForPlan("KIRO PRO")
	free := models.GetModelInfosForPlan("KIRO FREE")
	if !slices.Contains(modelIDs(pro), "claude-sonnet-4.5") || !slices.Contains(modelIDs(free), "claude-sonnet-4.5") {
		t.Fatal("alias exclusion fixture requires Claude Sonnet 4.5 in both plans")
	}
	withoutSonnet := modelIDs(pro, "claude-sonnet-4.5")
	for _, tc := range []struct {
		name, rule string
		want       []string
	}{
		{"pro excludes selected alias", "claude-sonnet-4-5", withoutSonnet},
		{"pro excludes source", "claude-sonnet-4.5", withoutSonnet},
		{"pro excludes alias wildcard", "claude-sonnet-4-5*", withoutSonnet},
		{"cleared rule restores models", "", modelIDs(pro)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := pluginapi.AuthModelRequest{AuthID: "pro", Host: host, StorageJSON: []byte(proModelStorage),
				Attributes: map[string]string{"excluded_models": tc.rule}}
			assertAccountModels(t, req, tc.want)
			// The same provider aliases must not block another account.
			assertAccountModels(t, pluginapi.AuthModelRequest{AuthID: "free", Host: host, StorageJSON: []byte(freeModelStorage)}, modelIDs(free))
		})
	}
}

func assertAccountModels(t *testing.T, req pluginapi.AuthModelRequest, want []string) {
	t.Helper()
	before, _ := json.Marshal(req)
	resp := callHandler[pluginapi.ModelResponse](t, pluginabi.MethodModelForAuth, req)
	if ids := modelIDs(resp.Models); !slices.Equal(ids, want) {
		t.Fatalf("account %s: models=%v, want %v", req.AuthID, ids, want)
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("model registration mutated the host configuration or credential")
	}
}

func TestAccountExclusionsRespectAliasScopeAndOverrides(t *testing.T) {
	infos := []pluginapi.ModelInfo{{ID: "claude-sonnet-4.5"}, {ID: "claude-sonnet-5.5"}, {ID: "auto"}}
	host := pluginapi.HostConfigSummary{
		ExcludedModels: map[string][]string{models.ProviderName: {"auto"}},
		OAuthModelAlias: map[string][]pluginapi.ModelAlias{
			models.ProviderName: {{Name: "claude-sonnet-4.5", Alias: "client-sonnet"}},
			"claude":            {{Name: "claude-sonnet-5.5", Alias: "other-provider"}},
		},
	}
	for _, tc := range []struct {
		name, rule, ownAliases string
		want                   []string
	}{
		{"global and account rules combine", " CLIENT-SONNET ", "", []string{"claude-sonnet-5.5"}},
		{"other provider does not leak", "other-provider", "", []string{"claude-sonnet-4.5", "claude-sonnet-5.5"}},
		{"account mapping overrides provider mapping", "client-sonnet", `[{"name":"claude-sonnet-5.5","alias":"client-sonnet"}]`, []string{"claude-sonnet-4.5"}},
		{"account mapping keeps other provider aliases", "client-sonnet", `[{"name":"claude-sonnet-5.5","alias":"pro-only"}]`, []string{"claude-sonnet-5.5"}},
		{"account mapping alias", "pro-only", `[{"name":"claude-sonnet-5.5","alias":"pro-only"}]`, []string{"claude-sonnet-4.5"}},
		{"malformed account mapping falls back", "client-sonnet", `{bad`, []string{"claude-sonnet-5.5"}},
		{"invalid mapping entries do not shadow provider mapping", "client-sonnet", `[{"name":"","alias":"client-sonnet"},{"name":"client-sonnet","alias":"client-sonnet"}]`, []string{"claude-sonnet-5.5"}},
		{"duplicate account alias uses first mapping", "pro-only", `[{"name":"claude-sonnet-5.5","alias":"pro-only"},{"name":"claude-sonnet-4.5","alias":"pro-only"}]`, []string{"claude-sonnet-4.5"}},
		{"no alias guessed from spelling", "claude-sonnet-4-5", "", []string{"claude-sonnet-4.5", "claude-sonnet-5.5"}},
		{"all excluded", "*", "", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := pluginapi.AuthModelRequest{Host: host, Attributes: map[string]string{"excluded_models": tc.rule, "model_aliases": tc.ownAliases}}
			if got := modelIDs(withoutModelExclusions(infos, req)); !slices.Equal(got, tc.want) {
				t.Fatalf("models=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestAccountExclusionClearsWithoutReusingStoredExclusion(t *testing.T) {
	host := pluginapi.HostConfigSummary{OAuthModelAlias: map[string][]pluginapi.ModelAlias{
		models.ProviderName: {{Name: "claude-sonnet-4.5", Alias: "sonnet-public"}},
	}}
	storage := []byte(`{"subscriptionTitle":"KIRO FREE","excluded_models":["sonnet-public"]}`)
	want := modelIDs(models.GetModelInfosForPlan("KIRO FREE"))
	// CPA's latest attributes have cleared the rule while storage still carries
	// an older snapshot. The plugin must not resurrect the removed exclusion.
	assertAccountModels(t, pluginapi.AuthModelRequest{StorageJSON: storage, Host: host,
		Attributes: map[string]string{"excluded_models": ""}}, want)
	// Alias matching is configuration-driven: after a mapping is changed, the
	// old alias rule must no longer disable an unrelated model.
	host.OAuthModelAlias[models.ProviderName][0].Alias = "renamed-sonnet"
	assertAccountModels(t, pluginapi.AuthModelRequest{StorageJSON: storage, Host: host,
		Attributes: map[string]string{"excluded_models": "sonnet-public"}}, want)
}

func TestModelsForAuthAppliesGlobalExclusions(t *testing.T) {
	host := pluginapi.HostConfigSummary{
		ExcludedModels:  map[string][]string{models.ProviderName: {" SONNET-PUBLIC ", "auto"}, "claude": {"*"}},
		OAuthModelAlias: map[string][]pluginapi.ModelAlias{models.ProviderName: {{Name: "claude-sonnet-4.5", Alias: "sonnet-public"}}},
	}
	assertAccountModels(t, pluginapi.AuthModelRequest{StorageJSON: []byte(proModelStorage), Host: host},
		modelIDs(models.GetModelInfosForPlan("KIRO PRO"), "auto", "claude-sonnet-4.5"))
}

func TestMatchWildcardMatchesCPA(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"glm-5", "glm-5", true},
		{"glm-5", "glm-5.1", false},
		{"claude-*", "claude-opus-4.8", true},
		{"*-4.8", "claude-opus-4.8", true},
		{"claude-*-4.8", "claude-opus-4.8", true},
		{"claude-*-4.8", "claude-opus-4.6", false},
		{"*", "anything", true},
		{"", "auto", false},
	}
	for _, tc := range cases {
		if got := matchWildcard(tc.pattern, tc.value); got != tc.want {
			t.Fatalf("matchWildcard(%q, %q) = %v", tc.pattern, tc.value, got)
		}
	}
}
