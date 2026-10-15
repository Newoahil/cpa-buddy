package collect

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/cpa"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/omc"
)

// fakeCredLister stands in for *cpa.Client's credential list.
type fakeCredLister struct {
	creds []domain.Credential
	err   error
}

func (f fakeCredLister) ListCredentials(context.Context) ([]domain.Credential, error) {
	return f.creds, f.err
}

// fakeQuotaLister stands in for the OMC MCP client.
type fakeQuotaLister struct {
	items []omc.QuotaItem
	err   error
	calls int
}

func (f *fakeQuotaLister) ListQuotas(context.Context) ([]omc.QuotaItem, error) {
	f.calls++
	return f.items, f.err
}

func testCred(provider domain.ProviderKind, index, alias string) domain.Credential {
	return domain.Credential{
		Key:       string(provider) + "/" + index + "opaque",
		Provider:  provider,
		Alias:     alias,
		ShortID:   "****" + index,
		AuthIndex: index,
	}
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func TestOMCCollectorMapsIdentityAndWindows(t *testing.T) {
	observedMS := int64(1700000000000)
	resetMS := int64(1700003600000)
	creds := fakeCredLister{creds: []domain.Credential{
		testCred(domain.ProviderCodex, "a1", "work"),
		testCred(domain.ProviderClaude, "a2", "claude-External"),
	}}
	quotas := &fakeQuotaLister{items: []omc.QuotaItem{{
		AuthIndex:    "a1",
		Provider:     "codex",
		Status:       "warning",
		ObservedAtMS: observedMS,
		Plan:         &omc.Plan{PlanType: "plus", PlanLabel: "Plus", Tier: "premium"},
		Windows: []omc.Window{{
			ID: "five_hour", Label: "5小时用量上限 (5-Hour)", Kind: "five_hour",
			Scope: "standard", UsedPercent: f64(42.5), ResetAtMS: &resetMS,
			PeriodHours: f64(5), ResetAccuracy: "exact",
		}},
	}}}
	c := NewOMCCollector(creds, quotas)
	snaps, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d, want 2", len(snaps))
	}

	codex := snaps[0]
	if codex.Credential.Key != "codex/a1opaque" || codex.Credential.Alias != "work" || codex.Credential.AuthIndex != "a1" {
		t.Fatalf("identity not preserved: %+v", codex.Credential)
	}
	if codex.Source != domain.SourceOMC || !codex.OK || codex.Confidence != domain.ConfidenceReported {
		t.Fatalf("source/ok/confidence = %+v", codex)
	}
	if codex.Plan != "Plus" {
		t.Fatalf("plan = %q, want the OMC plan label", codex.Plan)
	}
	observed := time.UnixMilli(observedMS).UTC()
	if !codex.FetchedAt.Equal(observed) || !codex.LastSuccessAt.Equal(observed) {
		t.Fatalf("observation time not used: fetched=%v last=%v want=%v", codex.FetchedAt, codex.LastSuccessAt, observed)
	}
	if len(codex.Windows) != 1 {
		t.Fatalf("windows = %+v", codex.Windows)
	}
	w := codex.Windows[0]
	if w.Name != "codex/omc/five_hour" || w.Label != "5小时用量上限 (5-Hour)" {
		t.Fatalf("window name/label = %q/%q", w.Name, w.Label)
	}
	if w.Scope != domain.ScopeAccount || w.ScopeID != "" {
		t.Fatalf("standard scope must map to account: %+v", w)
	}
	if w.UsedPercent == nil || *w.UsedPercent != 42.5 {
		t.Fatalf("used = %+v", w.UsedPercent)
	}
	if w.ResetAt == nil || !w.ResetAt.Equal(time.UnixMilli(resetMS).UTC()) {
		t.Fatalf("reset = %+v", w.ResetAt)
	}
	if w.WindowSeconds != 5*3600 {
		t.Fatalf("window seconds = %d, want 18000", w.WindowSeconds)
	}

	claude := snaps[1]
	if claude.OK || claude.Failure != domain.FailureUnsupported || claude.Credential.Key != "claude/a2opaque" {
		t.Fatalf("missing credential must be unreported, not fabricated: %+v", claude)
	}
}

func TestOMCCollectorScopeMapping(t *testing.T) {
	creds := fakeCredLister{creds: []domain.Credential{testCred(domain.ProviderClaude, "a1", "work")}}
	quotas := &fakeQuotaLister{items: []omc.QuotaItem{{
		AuthIndex: "a1", Provider: "claude", Status: "healthy", ObservedAtMS: 1700000000000,
		Windows: []omc.Window{
			{ID: "seven_day_sonnet", Scope: "model", Model: "claude-3-5-sonnet", UsedPercent: f64(10)},
			{ID: "seven_day_opus", Scope: "model", UsedPercent: f64(20)},
			{ID: "grouped", Scope: "group", Model: "gpt-5", UsedPercent: f64(30)},
			{ID: "weird", Scope: "mystery", UsedPercent: f64(40)},
			{ID: "review", Scope: "code_review", UsedPercent: f64(50)},
		},
	}}}
	snaps, err := NewOMCCollector(creds, quotas).Collect(context.Background())
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snaps=%+v err=%v", snaps, err)
	}
	w := snaps[0].Windows
	if w[0].Scope != domain.ScopeModel || w[0].ScopeID != "claude-3-5-sonnet" {
		t.Fatalf("model scope = %+v", w[0])
	}
	if w[1].Scope != domain.ScopeModel || w[1].ScopeID != "seven_day_opus" {
		t.Fatalf("model scope without model falls back to id: %+v", w[1])
	}
	if w[2].Scope != domain.ScopeGroup || w[2].ScopeID != "gpt-5" {
		t.Fatalf("group scope = %+v", w[2])
	}
	if w[3].Scope != domain.ScopeUnknown || w[3].ScopeID != "" {
		t.Fatalf("unknown scope must stay unknown: %+v", w[3])
	}
	if w[4].Scope != domain.ScopeGroup || w[4].ScopeID != "code_review" {
		t.Fatalf("code_review scope = %+v", w[4])
	}
}

func TestOMCCollectorSkipsDisabledAndUnsupported(t *testing.T) {
	disabled := testCred(domain.ProviderCodex, "d1", "off")
	disabled.Disabled = true
	noIndex := testCred(domain.ProviderCodex, "", "anon")
	ollama := testCred(domain.ProviderOllama, "o1", "cloud")
	creds := fakeCredLister{creds: []domain.Credential{disabled, noIndex, ollama}}
	quotas := &fakeQuotaLister{}
	snaps, err := NewOMCCollector(creds, quotas).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("disabled/missing-index/non-CPA credentials must be skipped: %+v", snaps)
	}
}

func TestOMCCollectorStatusMapping(t *testing.T) {
	cases := []struct {
		name        string
		status      string
		windows     []omc.Window
		wantOK      bool
		wantStale   bool
		wantFailure domain.FailureKind
	}{
		{"healthy", "healthy", []omc.Window{{ID: "five_hour", Scope: "standard", UsedPercent: f64(10)}}, true, false, domain.FailureNone},
		{"stale keeps numbers", "stale", []omc.Window{{ID: "five_hour", Scope: "standard", UsedPercent: f64(10)}}, true, true, domain.FailureNone},
		{"error is a failure", "error", []omc.Window{{ID: "five_hour", Scope: "standard", UsedPercent: f64(10)}}, false, false, domain.FailureTransport},
		{"unpublished", "unpublished", nil, false, false, domain.FailureUnsupported},
		{"idle", "idle", nil, false, false, domain.FailureUnsupported},
		{"healthy without windows", "healthy", nil, false, false, domain.FailureUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creds := fakeCredLister{creds: []domain.Credential{testCred(domain.ProviderCodex, "a1", "work")}}
			quotas := &fakeQuotaLister{items: []omc.QuotaItem{{
				AuthIndex: "a1", Provider: "codex", Status: tc.status, ObservedAtMS: 1700000000000,
				Windows: tc.windows,
			}}}
			snaps, err := NewOMCCollector(creds, quotas).Collect(context.Background())
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(snaps) != 1 {
				t.Fatalf("snapshots = %+v", snaps)
			}
			s := snaps[0]
			if s.OK != tc.wantOK || s.Stale != tc.wantStale || s.Failure != tc.wantFailure {
				t.Fatalf("status %q -> ok=%v stale=%v failure=%q, want ok=%v stale=%v failure=%q",
					tc.status, s.OK, s.Stale, s.Failure, tc.wantOK, tc.wantStale, tc.wantFailure)
			}
			if !s.OK {
				for _, w := range s.Windows {
					if w.UsedPercent != nil {
						t.Fatalf("a failed snapshot must not carry usable numbers: %+v", s)
					}
				}
			}
		})
	}
}

func TestOMCCollectorOMCFailureReturnsError(t *testing.T) {
	creds := fakeCredLister{creds: []domain.Credential{testCred(domain.ProviderCodex, "a1", "work")}}
	quotas := &fakeQuotaLister{err: errors.New("omc: request failed")}
	_, err := NewOMCCollector(creds, quotas).Collect(context.Background())
	if err == nil {
		t.Fatal("an OMC failure must surface as a collector error so fallback can react")
	}
}

func TestOMCCollectorListFailureIsControlPlane(t *testing.T) {
	creds := fakeCredLister{err: errors.New("boom")}
	_, err := NewOMCCollector(creds, &fakeQuotaLister{}).Collect(context.Background())
	if !errors.Is(err, ErrControlPlane) {
		t.Fatalf("err = %v, want ErrControlPlane", err)
	}
}

// fakeCollector is a scriptable domain.Collector.
type fakeCollector struct {
	name  string
	snaps []domain.QuotaSnapshot
	err   error
}

func (f *fakeCollector) Name() string { return f.name }
func (f *fakeCollector) Collect(context.Context) ([]domain.QuotaSnapshot, error) {
	return f.snaps, f.err
}

func TestFallbackSwitchesAfterThresholdAndRecovers(t *testing.T) {
	primary := &fakeCollector{name: "omc", err: errors.New("omc: request failed")}
	fallbackSnap := domain.QuotaSnapshot{Credential: domain.Credential{Key: "k"}, OK: true, Source: domain.SourceCPA}
	fallback := &fakeCollector{name: "cpa", snaps: []domain.QuotaSnapshot{fallbackSnap}}
	fb := NewFallbackCollector(primary, fallback, 2, nil)

	// Below the threshold: the error is surfaced, CPA is not queried.
	if _, err := fb.Collect(context.Background()); err == nil {
		t.Fatal("first failure must surface as an error")
	}
	if fb.InFallback() {
		t.Fatal("must not be in fallback before the threshold")
	}
	// Reaching the threshold: CPA answers for this cycle.
	snaps, err := fb.Collect(context.Background())
	if err != nil || len(snaps) != 1 || snaps[0].Source != domain.SourceCPA {
		t.Fatalf("fallback snaps=%+v err=%v", snaps, err)
	}
	if !fb.InFallback() {
		t.Fatal("must report fallback at the threshold")
	}
	// OMC recovers: it is tried first and switches back automatically.
	primary.err = nil
	primary.snaps = []domain.QuotaSnapshot{{Credential: domain.Credential{Key: "k"}, OK: true, Source: domain.SourceOMC}}
	snaps, err = fb.Collect(context.Background())
	if err != nil || len(snaps) != 1 || snaps[0].Source != domain.SourceOMC {
		t.Fatalf("recovery snaps=%+v err=%v", snaps, err)
	}
	if fb.InFallback() {
		t.Fatal("must leave fallback after OMC recovers")
	}
}

func TestFallbackNameIsPrimary(t *testing.T) {
	fb := NewFallbackCollector(&fakeCollector{name: "omc"}, &fakeCollector{name: "cpa"}, 3, nil)
	if fb.Name() != "omc" {
		t.Fatalf("name = %q, want omc", fb.Name())
	}
}

func TestBuildSelectsCollectorBySource(t *testing.T) {
	cpaCfg := config.Config{CPABaseURL: "http://cpa:8317", CPAManagementKey: "k", CPATimeout: time.Second}
	got, fb := Build(cpaCfg, NewCPAClient(cpaCfg), nil)
	if len(got) != 1 || got[0].Name() != "cpa" || fb != nil {
		t.Fatalf("cpa mode collectors = %+v fallback=%v", got, fb)
	}

	omcCfg := config.Config{
		CPABaseURL: "http://cpa:8317", CPAManagementKey: "k", CPATimeout: time.Second,
		QuotaSource: config.QuotaSourceOMC, OMCMCPURL: "http://omc:8080/omc/api/mcp", OMCMCPKey: "key", OMCFallbackAfter: 3,
	}
	got, fb = Build(omcCfg, NewCPAClient(omcCfg), nil)
	if len(got) != 1 || got[0].Name() != "omc" || fb == nil || fb.Name() != "omc" {
		t.Fatalf("omc mode collectors = %+v fallback=%v", got, fb)
	}
	if fb.InFallback() {
		t.Fatal("a fresh fallback collector is not in fallback")
	}
}

// TestOMCCollectorEndToEnd wires a real cpa.Client credential list and a real
// omc.Client MCP client to assert the association end to end: the alias and the
// opaque credential key come from CPA, the quota numbers come from OMC.
func TestOMCCollectorEndToEnd(t *testing.T) {
	cpaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/credentials") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[{"auth_index":"a1","name":"work@example.com","provider":"codex"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(cpaSrv.Close)

	omcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(raw, &req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"structuredContent":{"status":"success","data":{"items":[{"auth_index":"a1","provider":"codex","status":"healthy","observed_at_ms":1700000000000,"windows":[{"id":"five_hour","scope":"standard","used_percent":12}]}]}},"isError":false}}`))
		}
	}))
	t.Cleanup(omcSrv.Close)

	client := cpa.NewWithOptions(cpaSrv.URL, "mgmt", 5*time.Second, cpa.Options{APIVersion: "v8"})
	c := NewOMCCollector(client, omc.New(omcSrv.URL, "readonly", 5*time.Second))
	snaps, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("snapshots = %+v", snaps)
	}
	s := snaps[0]
	if s.Credential.Key == "" || s.Credential.Alias != "work" || s.Credential.Provider != domain.ProviderCodex {
		t.Fatalf("identity must come from CPA: %+v", s.Credential)
	}
	if s.Source != domain.SourceOMC || !s.OK || len(s.Windows) != 1 || s.Windows[0].Name != "codex/omc/five_hour" {
		t.Fatalf("quota must come from OMC: %+v", s)
	}
}

// TestFallbackOutageSinceTracksFirstFailureAndClears covers the OMC outage
// signal: the first failure of a run opens the outage, only the declared
// providers are outaged, RestoreOutage only bridges the start earlier, and
// recovery clears it.
func TestFallbackOutageSinceTracksFirstFailureAndClears(t *testing.T) {
	primary := &fakeCollector{name: "omc", err: errors.New("omc down")}
	fb := NewFallbackCollector(primary, &fakeCollector{name: "cpa"}, 3, nil)
	fb.SetOutageProviders(domain.ProviderCodex, domain.ProviderClaude)
	start := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	fb.now = func() time.Time { return start }

	if _, active := fb.OutageSince(domain.ProviderCodex, start); active {
		t.Fatal("no outage before any failure")
	}
	if _, err := fb.Collect(context.Background()); err == nil {
		t.Fatal("first failure must surface as an error below the threshold")
	}
	since, active := fb.OutageSince(domain.ProviderCodex, start)
	if !active || !since.Equal(start) {
		t.Fatalf("outage since=%v active=%v, want %v true", since, active, start)
	}
	if _, active := fb.OutageSince(domain.ProviderOllama, start); active {
		t.Fatal("a provider this source does not speak for must not be outaged")
	}

	earlier := start.Add(-2 * time.Hour)
	fb.RestoreOutage(earlier)
	if since, _ := fb.OutageSince(domain.ProviderCodex, start); !since.Equal(earlier) {
		t.Fatalf("RestoreOutage did not bridge the start: %v", since)
	}
	fb.RestoreOutage(start.Add(time.Hour)) // later: must be ignored
	if since, _ := fb.OutageSince(domain.ProviderCodex, start); !since.Equal(earlier) {
		t.Fatalf("RestoreOutage moved the start later: %v", since)
	}

	primary.err = nil
	if _, err := fb.Collect(context.Background()); err != nil {
		t.Fatalf("recovery Collect: %v", err)
	}
	if _, active := fb.OutageSince(domain.ProviderCodex, start); active {
		t.Fatal("outage must clear on recovery")
	}
	if !fb.OutageStart().IsZero() {
		t.Fatal("OutageStart must be zero after recovery")
	}
}
