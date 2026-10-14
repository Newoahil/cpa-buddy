package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// baseEnv sets the minimum required environment for Load to succeed.
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CPA_BASE_URL", "http://cpa:8317")
	t.Setenv("CPA_MANAGEMENT_KEY", "k")
	t.Setenv("TZ_NAME", "UTC")
	// Clear anything an ambient environment might inject.
	for _, k := range []string{
		"QUOTA_THRESHOLDS_JSON", "QUOTA_THRESHOLD_NOTICE", "QUOTA_THRESHOLD_WARN",
		"QUOTA_THRESHOLD_URGENT", "OLLAMA_ACCOUNTS_JSON", "POLL_INTERVAL",
		"QUOTA_HISTORY_DIR", "QUOTA_HISTORY_RETENTION", "FEISHU_ENABLED", "ANOMALY_FAILURE_RATE",
		"CPA_API_VERSION", "CPA_QUOTA_STRATEGY", "ANTIGRAVITY_QUOTA_PROFILE",
		"CPA_CONTEXT_OVERRIDES_JSON", "CARD_CHARTS_ENABLED",
		"COOLDOWN_POLL_INTERVAL", "COOLDOWN_ALERT_AFTER",
		"QUOTA_SOURCE", "OMC_MCP_URL", "OMC_MCP_KEY", "OMC_FALLBACK_AFTER",
	} {
		t.Setenv(k, "")
	}
}

func TestProviderOverrideWhitelist(t *testing.T) {
	for _, name := range []string{"codex", "claude", "gemini-cli", "antigravity", "ollama", "cluade", "gemini", "Codex", "unknown"} {
		t.Run(name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("QUOTA_THRESHOLDS_JSON", `{"`+name+`":{"notice":80}}`)
			_, err := Load()
			valid := name == "codex" || name == "claude" || name == "gemini-cli" || name == "antigravity" || name == "ollama"
			if (err == nil) != valid {
				t.Fatalf("provider %q err=%v", name, err)
			}
		})
	}
}

func TestLoadContextOverrides(t *testing.T) {
	baseEnv(t)
	t.Setenv("CPA_CONTEXT_OVERRIDES_JSON", `{"antigravity/0123456789abcdef":{"project_id":"verified-project"}}`)
	cfg, err := Load()
	if err != nil || cfg.CPAContextOverrides["antigravity/0123456789abcdef"].ProjectID != "verified-project" {
		t.Fatal(err)
	}
	t.Setenv("CPA_CONTEXT_OVERRIDES_JSON", `{"antigravity/0123456789abcdef":{"token":"secret"}}`)
	if _, err := Load(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("invalid override accepted or leaked")
	}
}

func TestLoadDefaults(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultThresholds != (Thresholds{Notice: 90, Warn: 95, Urgent: 100}) {
		t.Errorf("defaults = %+v", cfg.DefaultThresholds)
	}
	if cfg.HistoryRetention != DefaultHistoryRetention {
		t.Errorf("history retention = %v, want %v", cfg.HistoryRetention, DefaultHistoryRetention)
	}
	// The history dir defaults to a sibling of the state file.
	if cfg.HistoryDir != filepath.Join(filepath.Dir(cfg.StatePath), "history") {
		t.Errorf("history dir = %q", cfg.HistoryDir)
	}
	if cfg.CPAAPIVersion != "auto" || cfg.CPAQuotaStrategy != "auto" || cfg.AntigravityQuotaProfile != "current" {
		t.Fatal("wrong compatibility defaults")
	}
	// Charts are opt-in: the default card is text, which is compact and known
	// to render.
	if cfg.CardChartsEnabled {
		t.Error("CARD_CHARTS_ENABLED should default to false")
	}
	t.Setenv("CARD_CHARTS_ENABLED", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with charts on: %v", err)
	}
	if !cfg.CardChartsEnabled {
		t.Error("CARD_CHARTS_ENABLED=true was not honoured")
	}
}

// TestPartialOverrideKeepsDefaults is the core regression: a partial override
// must merge, not zero the missing fields.
func TestPartialOverrideKeepsDefaults(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"codex":{"notice":80}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.ThresholdsFor(domain.ProviderCodex)
	want := Thresholds{Notice: 80, Warn: 95, Urgent: 100}
	if got != want {
		t.Fatalf("merged thresholds = %+v, want %+v (missing fields must keep defaults)", got, want)
	}
}

func TestOverrideMergesSomeFields(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"ollama":{"notice":70,"warn":88}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.ThresholdsFor(domain.ProviderOllama)
	want := Thresholds{Notice: 70, Warn: 88, Urgent: 100}
	if got != want {
		t.Fatalf("merged thresholds = %+v, want %+v", got, want)
	}
}

func TestInvalidThresholdOrderingRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"codex":{"notice":95,"warn":80}}`)
	if _, err := Load(); err == nil {
		t.Fatal("expected error for notice > warn")
	}
}

func TestThresholdOutOfRangeRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"codex":{"urgent":150}}`)
	if _, err := Load(); err == nil {
		t.Fatal("expected error for urgent > 100")
	}
}

func TestNegativeThresholdRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_THRESHOLD_NOTICE", "-1")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for negative default threshold")
	}
}

func TestDuplicateOllamaNamesRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("OLLAMA_ACCOUNTS_JSON", `[
		{"name":"work","secure_session":"a"},
		{"name":"work","secure_session":"b"}
	]`)
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for duplicate Ollama account names")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention duplicate, got: %v", err)
	}
}

func TestInvalidEnvValueIsErrorNotFallback(t *testing.T) {
	cases := []struct {
		key, value string
	}{
		{"POLL_INTERVAL", "not-a-duration"},
		{"CPA_TIMEOUT", "banana"},
		{"STALE_AFTER_FAILURES", "many"},
		{"ANOMALY_FAILURE_RATE", "high"},
		{"FEISHU_ENABLED", "yes-please"},
		{"QUOTA_HISTORY_RETENTION", "0s"},
		{"QUOTA_HISTORY_RETENTION", "forever"},
		{"CPA_API_VERSION", "v7"},
		{"CPA_QUOTA_STRATEGY", "guess"},
		{"ANTIGRAVITY_QUOTA_PROFILE", "fallback"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			baseEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s=%q, got nil (silent fallback)", tc.key, tc.value)
			}
		})
	}
}

func TestValidEnvValuesParse(t *testing.T) {
	baseEnv(t)
	t.Setenv("POLL_INTERVAL", "30m")
	t.Setenv("QUOTA_HISTORY_DIR", "/custom/history")
	t.Setenv("QUOTA_HISTORY_RETENTION", "720h")
	t.Setenv("FEISHU_ENABLED", "true")
	t.Setenv("FEISHU_APP_ID", "id")
	t.Setenv("FEISHU_APP_SECRET", "secret")
	t.Setenv("FEISHU_CHAT_ID", "chat")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollInterval != 30*time.Minute || cfg.HistoryRetention != 720*time.Hour || cfg.HistoryDir != "/custom/history" {
		t.Errorf("intervals = %v / %v / %q", cfg.PollInterval, cfg.HistoryRetention, cfg.HistoryDir)
	}
	if !cfg.FeishuEnabled {
		t.Error("feishu should be enabled")
	}
}

func TestCooldownDefaults(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CooldownPollInterval != 60*time.Second || cfg.CooldownAlertAfter != 30*time.Minute {
		t.Fatalf("defaults = %v / %v", cfg.CooldownPollInterval, cfg.CooldownAlertAfter)
	}
}

func TestCooldownValuesParse(t *testing.T) {
	cases := []struct {
		poll, after string
		wantPoll    time.Duration
		wantAfter   time.Duration
	}{
		{"15s", "10m", 15 * time.Second, 10 * time.Minute},
		{"2m", "90s", 2 * time.Minute, 90 * time.Second},
		// 0 disables the watcher.
		{"0", "", 0, 30 * time.Minute},
		{"0s", "1m", 0, time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.poll+"/"+tc.after, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("COOLDOWN_POLL_INTERVAL", tc.poll)
			t.Setenv("COOLDOWN_ALERT_AFTER", tc.after)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.CooldownPollInterval != tc.wantPoll || cfg.CooldownAlertAfter != tc.wantAfter {
				t.Fatalf("got %v / %v", cfg.CooldownPollInterval, cfg.CooldownAlertAfter)
			}
		})
	}
}

func TestCooldownInvalidValuesRejected(t *testing.T) {
	cases := []struct{ key, value string }{
		{"COOLDOWN_POLL_INTERVAL", "14s"}, // below the 15s floor
		{"COOLDOWN_POLL_INTERVAL", "1ms"},
		{"COOLDOWN_POLL_INTERVAL", "-1m"},
		{"COOLDOWN_POLL_INTERVAL", "soon"},
		{"COOLDOWN_ALERT_AFTER", "0"},
		{"COOLDOWN_ALERT_AFTER", "-5m"},
		{"COOLDOWN_ALERT_AFTER", "later"},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			baseEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s=%q", tc.key, tc.value)
			}
		})
	}
}

func TestUnknownProviderOverrideRejected(t *testing.T) {
	baseEnv(t)
	// A provider key with a bad ordering is caught even if it is not one of the
	// built-in providers: better to reject than store an unusable value.
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"unknown":{"notice":99,"warn":1}}`)
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid unknown-provider override")
	}
}

func TestCompatibilityOptionsAndGeminiThreshold(t *testing.T) {
	baseEnv(t)
	t.Setenv("CPA_API_VERSION", "v0")
	t.Setenv("CPA_QUOTA_STRATEGY", "proxy")
	t.Setenv("ANTIGRAVITY_QUOTA_PROFILE", "legacy")
	t.Setenv("QUOTA_THRESHOLDS_JSON", `{"gemini-cli":{"notice":80}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CPAAPIVersion != "v0" || cfg.CPAQuotaStrategy != "proxy" || cfg.AntigravityQuotaProfile != "legacy" || cfg.ThresholdsFor(domain.ProviderGeminiCLI).Notice != 80 {
		t.Fatal("compatibility options not wired")
	}
}

// TestQuotaSourceDefaultsToCPA pins the backwards-compatible default: an
// existing deployment that sets nothing keeps querying CPA directly.
func TestQuotaSourceDefaultsToCPA(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.QuotaSource != QuotaSourceCPA {
		t.Fatalf("QuotaSource = %q, want %q", cfg.QuotaSource, QuotaSourceCPA)
	}
	if cfg.OMCFallbackAfter != DefaultOMCFallbackAfter {
		t.Fatalf("OMCFallbackAfter = %d, want %d", cfg.OMCFallbackAfter, DefaultOMCFallbackAfter)
	}
}

func TestQuotaSourceOMCConfigParses(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_SOURCE", "omc")
	t.Setenv("OMC_MCP_URL", "http://omc:8080/omc/api/mcp")
	t.Setenv("OMC_MCP_KEY", "read-only-secret")
	t.Setenv("OMC_FALLBACK_AFTER", "5")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.QuotaSource != QuotaSourceOMC || cfg.OMCMCPURL != "http://omc:8080/omc/api/mcp" || cfg.OMCMCPKey != "read-only-secret" || cfg.OMCFallbackAfter != 5 {
		t.Fatalf("omc config not wired: %+v", cfg)
	}
}

func TestQuotaSourceInvalidRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_SOURCE", "omc-typo")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unknown QUOTA_SOURCE")
	}
}

// TestQuotaSourceOMCRequiresEndpointAndKey: opting into OMC without an endpoint
// or key must fail loudly rather than silently reading nothing.
func TestQuotaSourceOMCRequiresEndpointAndKey(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		key     string
		wantErr bool
	}{
		{"missing both", "", "", true},
		{"missing key", "http://omc:8080/omc/api/mcp", "", true},
		{"missing url", "", "k", true},
		{"relative url", "omc:8080/omc/api/mcp", "k", true},
		{"complete", "http://omc:8080/omc/api/mcp", "k", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("QUOTA_SOURCE", "omc")
			t.Setenv("OMC_MCP_URL", tc.url)
			t.Setenv("OMC_MCP_KEY", tc.key)
			_, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestOMCKeyNeverLeaksInErrors: a validation failure must name the setting, not
// echo the secret value into a log or an operator's terminal.
func TestOMCKeyNeverLeaksInErrors(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_SOURCE", "omc")
	t.Setenv("OMC_MCP_URL", "not-a-url")
	t.Setenv("OMC_MCP_KEY", "super-secret-readonly-key")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid OMC_MCP_URL")
	}
	if strings.Contains(err.Error(), "super-secret-readonly-key") || strings.Contains(err.Error(), "not-a-url") {
		t.Fatalf("error leaked a secret/url value: %v", err)
	}
	if !strings.Contains(err.Error(), "OMC_MCP_URL") {
		t.Fatalf("error should name the offending setting: %v", err)
	}
}

func TestOMCFallbackAfterInvalidRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("QUOTA_SOURCE", "omc")
	t.Setenv("OMC_MCP_URL", "http://omc:8080/omc/api/mcp")
	t.Setenv("OMC_MCP_KEY", "k")
	t.Setenv("OMC_FALLBACK_AFTER", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for OMC_FALLBACK_AFTER=0")
	}
	t.Setenv("OMC_FALLBACK_AFTER", "many")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for a non-integer OMC_FALLBACK_AFTER")
	}
}
