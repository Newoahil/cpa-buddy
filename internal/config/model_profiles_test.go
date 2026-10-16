package config

import (
	"strings"
	"testing"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

func TestModelProfilesLoad(t *testing.T) {
	cases := []struct {
		name, raw string
		wantErr   bool
	}{
		{"empty", "", false},
		{"valid", `[{"name":"Daily","provider":"codex","windows":["5h"]}]`, false},
		{"all providers", `[{"name":"a","provider":"codex"},{"name":"b","provider":"claude"},{"name":"c","provider":"antigravity"},{"name":"d","provider":"gemini-cli"},{"name":"e","provider":"ollama"}]`, false},
		{"invalid JSON", `{`, true},
		{"null", `null`, true},
		{"wrong shape", `{}`, true},
		{"unknown field", `[{"name":"A","provider":"codex","windwos":["5h"]}]`, true},
		{"duplicate", `[{"name":"A","provider":"codex"},{"name":"A","provider":"claude"}]`, true},
		{"unknown provider", `[{"name":"A","provider":"wrong"}]`, true},
		{"empty name", `[{"name":" ","provider":"codex"}]`, true},
		{"empty window", `[{"name":"A","provider":"codex","windows":[""]}]`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("MODEL_PROFILES_JSON", tc.raw)
			cfg, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load error = %v", err)
			}
			if err != nil && !strings.Contains(err.Error(), "MODEL_PROFILES_JSON") {
				t.Fatalf("error = %v", err)
			}
			if tc.name == "valid" && (len(cfg.ModelProfiles) != 1 || cfg.ModelProfiles[0].Provider != domain.ProviderCodex || cfg.ModelProfiles[0].Windows[0] != "5h") {
				t.Fatalf("profiles = %+v", cfg.ModelProfiles)
			}
			if tc.name == "empty" && len(cfg.ModelProfiles) != 0 {
				t.Fatalf("profiles = %+v", cfg.ModelProfiles)
			}
		})
	}
}
