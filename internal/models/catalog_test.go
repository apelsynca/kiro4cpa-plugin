package models

import "testing"

func TestCommercialPlanCatalogs(t *testing.T) {
	for _, tc := range []struct {
		plan    string
		premium bool
	}{
		{"", false}, {"KIRO FREE", false}, {"unrecognized", false},
		{"KIRO PRO", true}, {"KIRO PRO+", true}, {"Kiro Pro Max", true}, {"Kiro Power", true},
		{"KIRO STUDENT", true}, {"  Kiro   Students ", true},
	} {
		ids := map[string]bool{}
		for _, m := range GetModelInfosForPlan(tc.plan) {
			if ids[m.ID] {
				t.Errorf("%q: duplicate ID %s", tc.plan, m.ID)
			}
			ids[m.ID] = true
		}
		if !ids["auto"] || !ids["claude-sonnet-4.5"] || ids["claude-sonnet-4"] || ids["claude-fable-5.1"] {
			t.Errorf("%q: invalid public catalog %v", tc.plan, ids)
		}
		for _, id := range []string{"gpt-5.6-sol", "claude-haiku-5.5", "claude-haiku-4.5", "grok-4.7", "kimi-k3"} {
			if ids[id] != tc.premium {
				t.Errorf("%q: model %s registered=%t; want %t", tc.plan, id, ids[id], tc.premium)
			}
		}
	}
}
