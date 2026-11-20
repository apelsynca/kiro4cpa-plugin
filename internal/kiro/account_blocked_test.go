package kiro

import (
	"strings"
	"testing"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func refreshAccount(t *testing.T, storage string, metadata map[string]any) (pluginapi.AuthRefreshResponse, map[string]any) {
	t.Helper()
	stubHTTP(t, func(r hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		if !strings.HasSuffix(r.URL, "/refreshToken") {
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL)
		}
		return ok(`{"accessToken":"at2","expiresIn":3600}`)
	})
	resp := callHandler[pluginapi.AuthRefreshResponse](t, pluginabi.MethodAuthRefresh,
		authRefreshRPC{AuthRefreshRequest: pluginapi.AuthRefreshRequest{AuthID: "k.json", AuthProvider: "kiro4cpa", StorageJSON: []byte(storage), Metadata: metadata}})
	fields, err := decodeFields(resp.Auth.StorageJSON)
	if err != nil {
		t.Fatal(err)
	}
	return resp, fields
}

func TestRefreshPreservesAccountControls(t *testing.T) {
	storage := strings.Replace(kiroRsSocial, `{`, `{"note":"Primary account","excluded_models":["auto"],`, 1)
	resp, fields := refreshAccount(t, storage, map[string]any{"disabled": true, "note": "Panel note"})
	if !resp.Auth.Disabled || resp.Auth.Metadata["disabled"] != true || resp.Auth.Metadata["note"] != "Panel note" {
		t.Fatalf("host account controls lost: disabled=%v metadata=%v", resp.Auth.Disabled, resp.Auth.Metadata)
	}
	excluded, _ := fields["excluded_models"].([]any)
	if fields["note"] != "Primary account" || len(excluded) != 1 || excluded[0] != "auto" {
		t.Fatalf("stored account controls lost: note=%v exclusions=%v", fields["note"], excluded)
	}
}

func TestRefreshClearsLegacyBlockedNote(t *testing.T) {
	storage := strings.Replace(kiroRsSocial, `{`, `{"note":"`+blockedNote+`","`+blockedAtKey+`":"2026-10-24T00:00:00Z",`, 1)
	resp, fields := refreshAccount(t, storage, nil)
	if fields["note"] != "" || fields[blockedAtKey] != nil || resp.Auth.Metadata["note"] != "" {
		t.Fatalf("note %v blockedAt %v meta %v", fields["note"], fields[blockedAtKey], resp.Auth.Metadata["note"])
	}
}

func TestRefreshDoesNotReenableDisabledAccount(t *testing.T) {
	storage := strings.Replace(kiroRsSocial, `"disabled":false`, `"disabled":true,"note":"`+blockedNote+`","`+blockedAtKey+`":"2026-10-24T00:00:00Z"`, 1)
	resp, fields := refreshAccount(t, storage, nil)
	if !resp.Auth.Disabled || fields["disabled"] != true || fields["note"] != "" {
		t.Fatalf("disabled=%v persisted=%v note=%v", resp.Auth.Disabled, fields["disabled"], fields["note"])
	}
}
