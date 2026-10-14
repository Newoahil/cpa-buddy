// Package config loads runtime configuration from the environment.
//
// Deployment target is Dokploy: every value arrives as an environment variable
// and secrets come from the Dokploy secrets provider. Nothing here is persisted
// and no secret is ever logged or exposed through the status page.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/cpa"
	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// Thresholds are used-percentage trigger points for one provider.
type Thresholds struct {
	Notice float64 `json:"notice"`
	Warn   float64 `json:"warn"`
	Urgent float64 `json:"urgent"`
}

// thresholdOverride is the JSON shape accepted from QUOTA_THRESHOLDS_JSON.
//
// Every field is a pointer so a partial override can be merged onto the
// defaults. With value fields a missing key would silently decode to 0, and a
// provider override of `{"notice":80}` would turn warn/urgent into 0 and mark
// every non-negative usage as exhausted.
type thresholdOverride struct {
	Notice *float64 `json:"notice"`
	Warn   *float64 `json:"warn"`
	Urgent *float64 `json:"urgent"`
}

// OllamaAccount is one Ollama Cloud account watched through the settings page.
//
// SecureSession is the `__Secure-session` cookie value. It is a high-value
// secret: never log it, never render it, never persist it to the state file.
type OllamaAccount struct {
	Name          string `json:"name"`
	SecureSession string `json:"secure_session"`
}

// Config is the fully resolved runtime configuration.
type Config struct {
	// CPA management transport and independently selected quota strategy/profile.
	CPABaseURL              string
	CPAManagementKey        string
	CPATimeout              time.Duration
	CPAAPIVersion           string
	CPAQuotaStrategy        string
	AntigravityQuotaProfile string
	CPAContextOverrides     map[string]cpa.ContextOverride

	// Ollama Cloud accounts, scraped from the settings page.
	OllamaAccounts []OllamaAccount
	OllamaTimeout  time.Duration

	// Quota source. When QuotaSource is QuotaSourceOMC, CPA quota is read from
	// Oh My CPA's read-only Streamable HTTP MCP endpoint instead of querying
	// CPA's upstream quota directly, so OMC stays the only party actively
	// querying upstreams. The CPA credential list is still read (management
	// API, not an upstream quota query) for identity and aliases.
	QuotaSource string
	// OMCMCPURL is the OMC MCP endpoint, e.g. http://omc:8080/omc/api/mcp.
	OMCMCPURL string
	// OMCMCPKey is the read-only MCP bearer key. It is a secret: never logged,
	// never rendered, never persisted.
	OMCMCPKey string
	// OMCFallbackAfter is the number of consecutive OMC failures after which
	// quota collection falls back to querying CPA directly for a cycle.
	OMCFallbackAfter int

	// Polling and evaluation.
	PollInterval time.Duration
	// FastPollInterval replaces PollInterval while any account is past its
	// notice threshold, so the last stretch to empty is watched closely.
	// 0 disables it.
	FastPollInterval   time.Duration
	DefaultThresholds  Thresholds
	ProviderThresholds map[domain.ProviderKind]Thresholds
	StaleAfterFailures int

	// Anomaly attribution for plain timeouts.
	AnomalyConsecutive int
	AnomalyWindow      time.Duration
	AnomalyFailureRate float64
	AnomalyMinRequests int

	// Quota history. HistoryDir is the append-only JSONL store directory;
	// HistoryRetention is how long a day file is kept before it is deleted.
	HistoryDir       string
	HistoryRetention time.Duration

	// Rate-limit cooldown watcher. It polls only the CPA credential list.
	// CooldownPollInterval 0 disables the watcher; otherwise it is at least
	// MinCooldownPollInterval. A cooldown is alerted only when CPA's retry
	// time lies more than CooldownAlertAfter ahead, i.e. beyond CPA's own
	// backoff: the account is used up. Routine cooldowns only feed the digest.
	CooldownPollInterval time.Duration
	CooldownAlertAfter   time.Duration

	// Notification channels.
	FeishuEnabled   bool
	FeishuAppID     string
	FeishuAppSecret string
	FeishuChatID    string
	WebhookURL      string

	// QuotaIgnoredGroups lists model/group scopes to exclude from notifications.
	QuotaIgnoredGroups []string

	// Scheduling.
	Location     *time.Location
	SummaryTimes []string

	// Presentation.
	Tone string

	// Local state and HTTP surface.
	StatePath string
	HTTPAddr  string

	// CardChartsEnabled turns the Feishu card's progress chart on or off.
	//
	// It defaults to false: the text lines already carry the exact remaining
	// percentage, so a chart adds no information while costing a lot of
	// vertical space. It is kept as an opt-in for anyone who wants the picture.
	CardChartsEnabled bool
}

const (
	// ToneCasual is the team-voice default agreed for card copy.
	ToneCasual = "casual"
	// ToneFormal is the neutral ops-alert wording.
	ToneFormal = "formal"
	// DefaultThresholdNotice etc. are the fallbacks for absent override keys.
	DefaultThresholdNotice = 90.0
	DefaultThresholdWarn   = 95.0
	DefaultThresholdUrgent = 100.0
	// DefaultHistoryRetention keeps 90 days of quota history. It is expressed
	// as a duration so QUOTA_HISTORY_RETENTION=720h reads naturally.
	DefaultHistoryRetention = 2160 * time.Hour
	// DefaultCooldownPollInterval is how often the cooldown watcher lists
	// credentials; MinCooldownPollInterval is the floor for a non-zero value.
	DefaultCooldownPollInterval = 60 * time.Second
	MinCooldownPollInterval     = 15 * time.Second
	// DefaultCooldownAlertAfter is how far ahead CPA's retry time must lie for
	// a cooldown to be alerted. It matches CPA's longest own backoff (30m):
	// anything shorter is CPA's routine 429/5xx backoff and only feeds the
	// daily digest.
	DefaultCooldownAlertAfter = 30 * time.Minute
	// DefaultQuotaIgnoredGroups is the default comma-separated model groups to ignore in notifications.
	DefaultQuotaIgnoredGroups = "Claude and GPT models"
	// QuotaSourceCPA (the default) queries CPA's management API directly;
	// QuotaSourceOMC reads the quota OMC has collected instead. The OMC_MCP_*
	// settings are only required when QUOTA_SOURCE=omc.
	QuotaSourceCPA = "cpa"
	QuotaSourceOMC = "omc"
	// DefaultOMCFallbackAfter is how many consecutive OMC failures trigger a
	// direct CPA fallback: one hiccup is not enough to bypass OMC.
	DefaultOMCFallbackAfter = 3
)

// Load reads configuration from the process environment and validates it.
//
// Invalid values are errors, never silent fallbacks: a typo in a threshold or
// an interval must be visible at startup, because a silently-substituted default
// can change alerting behaviour without anyone noticing.
func Load() (Config, error) {
	var errs []string

	cfg := Config{
		CPABaseURL:              strings.TrimRight(env("CPA_BASE_URL", "http://cpa:8317"), "/"),
		CPAManagementKey:        env("CPA_MANAGEMENT_KEY", ""),
		CPAAPIVersion:           env("CPA_API_VERSION", "auto"),
		CPAQuotaStrategy:        env("CPA_QUOTA_STRATEGY", "auto"),
		AntigravityQuotaProfile: env("ANTIGRAVITY_QUOTA_PROFILE", "current"),
		FeishuEnabled:           false,
		FeishuAppID:             env("FEISHU_APP_ID", ""),
		FeishuAppSecret:         env("FEISHU_APP_SECRET", ""),
		FeishuChatID:            env("FEISHU_CHAT_ID", ""),
		WebhookURL:              env("WEBHOOK_URL", ""),
		Tone:                    env("TONE", ToneCasual),
		StatePath:               env("STATE_PATH", "/data/state.json"),
		HTTPAddr:                env("HTTP_ADDR", ":8080"),
	}

	cfg.CPATimeout = mustDuration(&errs, "CPA_TIMEOUT", 20*time.Second)
	cfg.OllamaTimeout = mustDuration(&errs, "OLLAMA_TIMEOUT", 20*time.Second)
	cfg.PollInterval = mustDuration(&errs, "POLL_INTERVAL", 15*time.Minute)
	cfg.FastPollInterval = mustOptionalDuration(&errs, "FAST_POLL_INTERVAL", 3*time.Minute)
	cfg.StaleAfterFailures = mustInt(&errs, "STALE_AFTER_FAILURES", 2)
	cfg.AnomalyConsecutive = mustInt(&errs, "ANOMALY_CONSECUTIVE", 3)
	cfg.AnomalyWindow = mustDuration(&errs, "ANOMALY_WINDOW", 5*time.Minute)
	cfg.AnomalyFailureRate = mustFloat(&errs, "ANOMALY_FAILURE_RATE", 0.2)
	cfg.AnomalyMinRequests = mustInt(&errs, "ANOMALY_MIN_REQUESTS", 5)
	// History lives beside the state file by default, so a single /data volume
	// covers both and nothing needs a second mount. It is still overridable.
	cfg.HistoryDir = env("QUOTA_HISTORY_DIR", filepath.Join(filepath.Dir(cfg.StatePath), "history"))
	cfg.HistoryRetention = mustDuration(&errs, "QUOTA_HISTORY_RETENTION", DefaultHistoryRetention)
	cfg.CooldownPollInterval = mustOptionalDuration(&errs, "COOLDOWN_POLL_INTERVAL", DefaultCooldownPollInterval)
	cfg.CooldownAlertAfter = mustDuration(&errs, "COOLDOWN_ALERT_AFTER", DefaultCooldownAlertAfter)
	cfg.FeishuEnabled = mustBool(&errs, "FEISHU_ENABLED", false)
	cfg.CardChartsEnabled = mustBool(&errs, "CARD_CHARTS_ENABLED", false)
	cfg.QuotaSource = env("QUOTA_SOURCE", QuotaSourceCPA)
	cfg.OMCMCPURL = env("OMC_MCP_URL", "")
	cfg.OMCMCPKey = env("OMC_MCP_KEY", "")
	cfg.OMCFallbackAfter = mustInt(&errs, "OMC_FALLBACK_AFTER", DefaultOMCFallbackAfter)

	cfg.DefaultThresholds = Thresholds{
		Notice: mustFloat(&errs, "QUOTA_THRESHOLD_NOTICE", DefaultThresholdNotice),
		Warn:   mustFloat(&errs, "QUOTA_THRESHOLD_WARN", DefaultThresholdWarn),
		Urgent: mustFloat(&errs, "QUOTA_THRESHOLD_URGENT", DefaultThresholdUrgent),
	}

	loc, err := time.LoadLocation(env("TZ_NAME", "Asia/Shanghai"))
	if err != nil {
		return Config{}, fmt.Errorf("load timezone: %w", err)
	}
	cfg.Location = loc

	cfg.SummaryTimes = splitList(env("SUMMARY_TIMES", "10:00,17:00"))
	cfg.QuotaIgnoredGroups = splitList(env("QUOTA_IGNORED_GROUPS", DefaultQuotaIgnoredGroups))

	if raw := env("OLLAMA_ACCOUNTS_JSON", ""); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.OllamaAccounts); err != nil {
			return Config{}, fmt.Errorf("parse OLLAMA_ACCOUNTS_JSON: %w", err)
		}
	}

	cfg.ProviderThresholds, err = parseProviderThresholds(env("QUOTA_THRESHOLDS_JSON", ""), cfg.DefaultThresholds)
	if err != nil {
		return Config{}, err
	}
	cfg.CPAContextOverrides, err = cpa.ParseContextOverrides(env("CPA_CONTEXT_OVERRIDES_JSON", ""))
	if err != nil {
		return Config{}, err
	}

	if len(errs) > 0 {
		return Config{}, errors.New("invalid configuration: " + strings.Join(errs, "; "))
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseProviderThresholds decodes a partial provider override and merges each
// field onto the defaults, then validates the merged result.
func parseProviderThresholds(raw string, defaults Thresholds) (map[domain.ProviderKind]Thresholds, error) {
	out := map[domain.ProviderKind]Thresholds{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	parsed := map[string]thresholdOverride{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parse QUOTA_THRESHOLDS_JSON: %w", err)
	}
	for name, ov := range parsed {
		switch domain.ProviderKind(name) {
		case domain.ProviderCodex, domain.ProviderClaude, domain.ProviderGeminiCLI, domain.ProviderAntigravity, domain.ProviderOllama:
		default:
			return nil, errors.New("QUOTA_THRESHOLDS_JSON contains an unknown provider")
		}
		merged := defaults
		if ov.Notice != nil {
			merged.Notice = *ov.Notice
		}
		if ov.Warn != nil {
			merged.Warn = *ov.Warn
		}
		if ov.Urgent != nil {
			merged.Urgent = *ov.Urgent
		}
		if err := validThresholds(merged); err != nil {
			return nil, fmt.Errorf("QUOTA_THRESHOLDS_JSON[%q]: %w", name, err)
		}
		out[domain.ProviderKind(name)] = merged
	}
	return out, nil
}

// validThresholds enforces the ordering and bounds for one threshold set.
func validThresholds(t Thresholds) error {
	for _, v := range []float64{t.Notice, t.Warn, t.Urgent} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("thresholds must be finite numbers")
		}
	}
	if !(0 <= t.Notice && t.Notice <= t.Warn && t.Warn <= t.Urgent && t.Urgent <= 100) {
		return fmt.Errorf("thresholds must satisfy 0 <= notice <= warn <= urgent <= 100 (got %g/%g/%g)", t.Notice, t.Warn, t.Urgent)
	}
	return nil
}

// ThresholdsFor returns the provider override when present, else the default.
func (c Config) ThresholdsFor(p domain.ProviderKind) Thresholds {
	if t, ok := c.ProviderThresholds[p]; ok {
		return t
	}
	return c.DefaultThresholds
}

func (c Config) validate() error {
	var errs []string
	if c.CPAAPIVersion != "auto" && c.CPAAPIVersion != "v0" && c.CPAAPIVersion != "v8" {
		errs = append(errs, "CPA_API_VERSION must be auto, v0 or v8")
	}
	if c.CPAQuotaStrategy != "auto" && c.CPAQuotaStrategy != "normalized" && c.CPAQuotaStrategy != "proxy" {
		errs = append(errs, "CPA_QUOTA_STRATEGY must be auto, normalized or proxy")
	}
	if c.AntigravityQuotaProfile != "current" && c.AntigravityQuotaProfile != "legacy" {
		errs = append(errs, "ANTIGRAVITY_QUOTA_PROFILE must be current or legacy")
	}
	if c.CPABaseURL == "" {
		errs = append(errs, "CPA_BASE_URL is required")
	}
	if c.CPAManagementKey == "" {
		errs = append(errs, "CPA_MANAGEMENT_KEY is required")
	}
	if c.QuotaSource != QuotaSourceCPA && c.QuotaSource != QuotaSourceOMC {
		errs = append(errs, "QUOTA_SOURCE must be cpa or omc")
	}
	if c.QuotaSource == QuotaSourceOMC {
		if c.OMCMCPURL == "" {
			errs = append(errs, "OMC_MCP_URL is required when QUOTA_SOURCE=omc")
		} else if u, err := url.Parse(c.OMCMCPURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			// Never echo the URL value: it may embed credentials in a userinfo
			// component. The setting name is enough to fix it.
			errs = append(errs, "OMC_MCP_URL must be an absolute http(s) URL")
		}
		if c.OMCMCPKey == "" {
			errs = append(errs, "OMC_MCP_KEY is required when QUOTA_SOURCE=omc")
		}
		if c.OMCFallbackAfter < 1 {
			errs = append(errs, "OMC_FALLBACK_AFTER must be at least 1")
		}
	}
	if c.PollInterval < time.Minute {
		errs = append(errs, "POLL_INTERVAL must be at least 1m")
	}
	if c.FastPollInterval != 0 && c.FastPollInterval < time.Minute {
		errs = append(errs, "FAST_POLL_INTERVAL must be 0 (disabled) or at least 1m")
	}
	if c.CooldownPollInterval != 0 && c.CooldownPollInterval < MinCooldownPollInterval {
		errs = append(errs, "COOLDOWN_POLL_INTERVAL must be 0 (disabled) or at least 15s")
	}
	if err := validThresholds(c.DefaultThresholds); err != nil {
		errs = append(errs, "default quota thresholds: "+err.Error())
	}
	for _, p := range []domain.ProviderKind{
		domain.ProviderCodex, domain.ProviderClaude, domain.ProviderAntigravity, domain.ProviderGeminiCLI, domain.ProviderOllama,
	} {
		if t, ok := c.ProviderThresholds[p]; ok {
			if err := validThresholds(t); err != nil {
				errs = append(errs, fmt.Sprintf("provider %q: %s", p, err))
			}
		}
	}
	if c.FeishuEnabled {
		if c.FeishuAppID == "" || c.FeishuAppSecret == "" {
			errs = append(errs, "FEISHU_APP_ID and FEISHU_APP_SECRET are required when FEISHU_ENABLED=true")
		}
		if c.FeishuChatID == "" {
			errs = append(errs, "FEISHU_CHAT_ID is required when FEISHU_ENABLED=true")
		}
	}
	seenNames := map[string]bool{}
	for i, a := range c.OllamaAccounts {
		if a.Name == "" || a.SecureSession == "" {
			errs = append(errs, fmt.Sprintf("OLLAMA_ACCOUNTS_JSON[%d] needs name and secure_session", i))
			continue
		}
		// Duplicate names would collapse two accounts onto one credential key
		// and make their quota indistinguishable.
		if seenNames[a.Name] {
			errs = append(errs, fmt.Sprintf("OLLAMA_ACCOUNTS_JSON has duplicate name %q", a.Name))
		}
		seenNames[a.Name] = true
	}
	for _, spec := range c.SummaryTimes {
		if _, err := time.Parse("15:04", spec); err != nil {
			errs = append(errs, fmt.Sprintf("SUMMARY_TIMES entry %q must be HH:MM", spec))
		}
	}
	if len(errs) > 0 {
		return errors.New("invalid configuration: " + strings.Join(errs, "; "))
	}
	return nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// mustDuration parses an optional duration. An unset variable yields def; a set
// but malformed or non-positive value is recorded as an error.
func mustDuration(errs *[]string, key string, def time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not a valid duration", key, raw))
		return def
	}
	if d <= 0 {
		*errs = append(*errs, fmt.Sprintf("%s must be positive", key))
		return def
	}
	return d
}

// mustOptionalDuration is mustDuration for a setting where 0 is meaningful
// (disabled). Negative or malformed values are still errors.
func mustOptionalDuration(errs *[]string, key string, def time.Duration) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not a valid duration", key, raw))
		return def
	}
	if d < 0 {
		*errs = append(*errs, fmt.Sprintf("%s must not be negative", key))
		return def
	}
	return d
}

func mustInt(errs *[]string, key string, def int) int {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not an integer", key, raw))
		return def
	}
	return v
}

func mustFloat(errs *[]string, key string, def float64) float64 {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not a number", key, raw))
		return def
	}
	return v
}

func mustBool(errs *[]string, key string, def bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not a boolean", key, raw))
		return def
	}
	return v
}

func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
