package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// TestLoadMissingFileIsEmpty covers the first-boot path: no file must not error
// and must report Bootstrapped=false.
func TestLoadMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(filepath.Join(dir, "nested", "state.json"))

	st, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if st == nil {
		t.Fatal("Load() returned nil state")
	}
	if st.Bootstrapped {
		t.Error("Bootstrapped = true, want false for missing file")
	}
	if st.Credentials == nil {
		t.Error("Credentials map must be initialised")
	}
	if len(st.Credentials) != 0 {
		t.Errorf("Credentials = %d entries, want 0", len(st.Credentials))
	}
}

// TestLoadEmptyFileIsNormal covers a zero-byte file, which some deployment
// mounts create before the first write.
func TestLoadEmptyFileIsNormal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := NewFileStore(path).Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if st.Bootstrapped || len(st.Credentials) != 0 {
		t.Errorf("empty file must yield empty state, got %+v", st)
	}
}

// TestSaveLoadRoundTrip covers serialisation of every field, including window
// maps, so restarts preserve the dedup memory.
func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewFileStore(path)

	reset := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	lastSuccess := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	want := &State{
		Bootstrapped: true,
		UpdatedAt:    lastSuccess,
		Credentials: map[string]CredentialRecord{
			"codex:alpha": {
				State:               domain.StateWarning,
				Quota:               QuotaWarning,
				Credential:          CredValid,
				Freshness:           Stale,
				LastUsedPercent:     map[string]float64{"5h": 42.5, "7d": 96.25},
				LastResetAt:         map[string]time.Time{"7d": reset},
				LastSuccessAt:       lastSuccess,
				ConsecutiveFailures: 2,
				ReachedNotice:       true,
			},
		},
	}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !got.Bootstrapped || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("header mismatch: got %+v", got)
	}
	rec, ok := got.Credentials["codex:alpha"]
	if !ok {
		t.Fatal("credential record lost in round trip")
	}
	if rec.State != domain.StateWarning {
		t.Errorf("State = %q, want %q", rec.State, domain.StateWarning)
	}
	if rec.LastUsedPercent["7d"] != 96.25 || rec.LastUsedPercent["5h"] != 42.5 {
		t.Errorf("LastUsedPercent = %v", rec.LastUsedPercent)
	}
	if !rec.LastResetAt["7d"].Equal(reset) {
		t.Errorf("LastResetAt[7d] = %v, want %v", rec.LastResetAt["7d"], reset)
	}
	if rec.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", rec.ConsecutiveFailures)
	}
	if rec.Quota != QuotaWarning || rec.Credential != CredValid || rec.Freshness != Stale {
		t.Errorf("dimensions lost: quota=%q credential=%q freshness=%q", rec.Quota, rec.Credential, rec.Freshness)
	}
	if !rec.ReachedNotice {
		t.Error("ReachedNotice lost in round trip")
	}
}

// TestLoadCorruptFileRecovers covers a truncated/garbled file: it must return an
// error (for logging) but still hand back a usable empty state so startup
// continues.
func TestLoadCorruptFileRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := NewFileStore(path).Load()
	if err == nil {
		t.Error("Load() error = nil, want an error for corrupt file")
	}
	if st == nil || len(st.Credentials) != 0 || st.Bootstrapped {
		t.Errorf("corrupt file must yield usable empty state, got %+v", st)
	}
}

// TestSaveIsAtomicAndCreatesParentDir verifies the temp-file+rename path and
// that a missing parent directory is created on demand.
func TestSaveIsAtomicAndCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deeper", "state.json")
	store := NewFileStore(path)

	if err := store.Save(&State{Bootstrapped: true, Credentials: map[string]CredentialRecord{}}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	// No leftover temp files in the target directory.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	// A second save overwrites cleanly (rename replaces).
	if err := store.Save(&State{Bootstrapped: true, UpdatedAt: time.Unix(5, 0).UTC(), Credentials: map[string]CredentialRecord{}}); err != nil {
		t.Fatalf("second Save() error = %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt.Unix() != 5 {
		t.Errorf("UpdatedAt = %v, want overwrite to take effect", got.UpdatedAt)
	}
}

// TestStateFileHasNoSecrets guards the contract that the persisted document
// never contains credential secrets even if a caller passes a fully populated
// domain.Credential alongside.
func TestStateFileHasNoSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// Sanity: what we persist is structurally incapable of carrying secrets.
	raw, err := json.Marshal(&State{Credentials: map[string]CredentialRecord{
		"ollama:work": {State: domain.StateHealthy},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"cookie", "secure_session", "token", "email", "management"} {
		if strings.Contains(strings.ToLower(string(raw)), banned) {
			t.Errorf("persisted state references %q", banned)
		}
	}
	if err := NewFileStore(path).Save(&State{Credentials: map[string]CredentialRecord{"ollama:work": {State: domain.StateHealthy}}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Error("saved state is not valid JSON")
	}
}

// ---------------------------------------------------------------------------
// Notification outbox
// ---------------------------------------------------------------------------

func alert(title string) domain.Alert {
	return domain.Alert{Kind: domain.AlertQuotaThreshold, Title: title}
}

func TestQueuePendingAppendsAndIsolatesChannels(t *testing.T) {
	st := Empty()
	st.QueuePending([]string{"feishu"}, []domain.Alert{alert("a")})
	st.QueuePending([]string{"feishu", "webhook"}, []domain.Alert{alert("b")})

	feishu := st.PendingFor("feishu")
	if len(feishu) != 2 || feishu[0].Title != "a" || feishu[1].Title != "b" {
		t.Fatalf("feishu queue = %+v", feishu)
	}
	hook := st.PendingFor("webhook")
	if len(hook) != 1 || hook[0].Title != "b" {
		t.Fatalf("webhook queue = %+v", hook)
	}
	if got := st.PendingFor("missing"); got != nil {
		t.Errorf("unknown channel = %+v, want nil", got)
	}
}

func TestQueuePendingTrimsOldest(t *testing.T) {
	st := Empty()
	for i := 0; i < maxPendingPerChannel+5; i++ {
		st.QueuePending([]string{"feishu"}, []domain.Alert{alert(string(rune('A' + i%26)))})
	}
	got := st.PendingFor("feishu")
	if len(got) != maxPendingPerChannel {
		t.Fatalf("queue length = %d, want %d", len(got), maxPendingPerChannel)
	}
}

func TestQueuePendingReturnsCopy(t *testing.T) {
	st := Empty()
	st.QueuePending([]string{"feishu"}, []domain.Alert{alert("a")})
	got := st.PendingFor("feishu")
	got[0].Title = "mutated"
	if again := st.PendingFor("feishu"); again[0].Title != "a" {
		t.Error("PendingFor leaked a mutable reference")
	}
}

func TestClearPending(t *testing.T) {
	st := Empty()
	st.QueuePending([]string{"feishu"}, []domain.Alert{alert("a")})
	st.ClearPending("feishu")
	if got := st.PendingFor("feishu"); got != nil {
		t.Errorf("queue not cleared: %+v", got)
	}
	// Clearing an unknown channel is a no-op.
	st.ClearPending("nope")
}

func TestPendingRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewFileStore(path)
	want := Empty()
	want.Bootstrapped = true
	want.QueuePending([]string{"feishu"}, []domain.Alert{alert("queued")})
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	queue := got.PendingFor("feishu")
	if len(queue) != 1 || queue[0].Title != "queued" {
		t.Fatalf("pending lost in round trip: %+v", got.Pending)
	}
}

// ---------------------------------------------------------------------------
// Pending items and the quiet-hours queue
// ---------------------------------------------------------------------------

func TestPendingItemRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewFileStore(path)
	st := Empty()
	st.Bootstrapped = true
	st.OpenPending(PendingItem{
		ID: "credential_invalid:abc", Kind: "credential_invalid",
		Provider: domain.ProviderClaude, CredKey: "claude-1", Alias: "claude-1",
		Action: "重新登录", Status: PendingOpen,
	})
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	it, ok := got.PendingItem("credential_invalid:abc")
	if !ok || it.Provider != domain.ProviderClaude || it.CredKey != "claude-1" {
		t.Fatalf("pending item lost in round trip: %+v", got.PendingItems)
	}
}

// TestOpenPendingPreservesSinceAndNotified checks that re-observing an ongoing
// problem never resets its age or re-arms its notification.
func TestOpenPendingPreservesSinceAndNotified(t *testing.T) {
	st := Empty()
	since := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	notified := since.Add(time.Minute)
	st.PendingItems = []PendingItem{{ID: "x", Kind: "monitor_down", Since: since, NotifiedAt: notified, Status: PendingOpen}}
	st.OpenPending(PendingItem{ID: "x", Kind: "monitor_down", LastSeen: since.Add(time.Hour)})
	it, _ := st.PendingItem("x")
	if !it.Since.Equal(since) {
		t.Errorf("Since reset: %v want %v", it.Since, since)
	}
	if !it.NotifiedAt.Equal(notified) {
		t.Errorf("NotifiedAt reset: %v want %v", it.NotifiedAt, notified)
	}
}

func TestResolvePendingAndPrune(t *testing.T) {
	st := Empty()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	st.PendingItems = []PendingItem{
		{ID: "old", Status: PendingResolved, ResolvedAt: now.Add(-8 * 24 * time.Hour)},
		{ID: "recent", Status: PendingResolved, ResolvedAt: now.Add(-1 * time.Hour)},
		{ID: "open", Status: PendingOpen, Since: now.Add(-100 * time.Hour)},
	}
	st.PruneResolved(now)
	if _, ok := st.PendingItem("old"); ok {
		t.Error("resolved item older than 7 days should be pruned")
	}
	if _, ok := st.PendingItem("recent"); !ok {
		t.Error("recently resolved item must be kept")
	}
	if _, ok := st.PendingItem("open"); !ok {
		t.Error("open item must never be pruned")
	}

	st.ResolvePending("open", now)
	it, _ := st.PendingItem("open")
	if it.Status != PendingResolved || !it.ResolvedAt.Equal(now) {
		t.Errorf("resolve failed: %+v", it)
	}
}

func TestQuietQueueRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewFileStore(path)
	st := Empty()
	st.QueueQuiet([]domain.Alert{{Kind: domain.AlertCredential, Title: "a"}})
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	queued := got.TakeQuiet()
	if len(queued) != 1 || queued[0].Title != "a" {
		t.Fatalf("quiet queue lost: %+v", got.QuietQueue)
	}
	if again := got.TakeQuiet(); again != nil {
		t.Fatalf("TakeQuiet must clear the queue, got %+v", again)
	}
}

// TestOldStateFileLoadsWithoutNewFields is the compatibility check: a state
// file written before pending items existed must load cleanly.
func TestOldStateFileLoadsWithoutNewFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	legacy := `{"bootstrapped":true,"updated_at":"2026-10-01T00:00:00Z","credentials":{"claude-1":{"state":"healthy"}}}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := NewFileStore(path).Load()
	if err != nil {
		t.Fatalf("legacy file must load: %v", err)
	}
	if !st.Bootstrapped || len(st.Credentials) != 1 {
		t.Fatalf("legacy fields lost: %+v", st)
	}
	if st.PendingItems != nil || st.QuietQueue != nil {
		t.Errorf("new fields should default to nil, got %+v / %+v", st.PendingItems, st.QuietQueue)
	}
	if st.ChannelTracks != nil {
		t.Errorf("channel tracks should default to nil, got %+v", st.ChannelTracks)
	}
	if !st.OMCOutageSince.IsZero() {
		t.Errorf("OMCOutageSince should default to zero, got %v", st.OMCOutageSince)
	}
}

// TestChannelTrackingRoundTrip covers the monitor_down / omc_outage memory.
func TestChannelTrackingRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store := NewFileStore(path)
	firstSeen := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	lastValid := time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)
	outage := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

	st := Empty()
	st.Bootstrapped = true
	st.ChannelTracks = map[domain.ProviderKind]ChannelTrack{
		domain.ProviderCodex:  {FirstSeen: firstSeen, LastValid: lastValid},
		domain.ProviderOllama: {FirstSeen: firstSeen},
	}
	st.OMCOutageSince = outage
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !got.OMCOutageSince.Equal(outage) {
		t.Fatalf("OMCOutageSince = %v, want %v", got.OMCOutageSince, outage)
	}
	track, ok := got.ChannelTracks[domain.ProviderCodex]
	if !ok || !track.FirstSeen.Equal(firstSeen) || !track.LastValid.Equal(lastValid) {
		t.Fatalf("codex track = %+v", track)
	}
	if oll := got.ChannelTracks[domain.ProviderOllama]; !oll.LastValid.IsZero() {
		t.Fatalf("never-valid track must keep LastValid zero: %+v", oll)
	}
}

func TestProjectPriority(t *testing.T) {
	cases := []struct {
		name string
		q    QuotaLevel
		c    CredentialHealth
		f    Freshness
		want domain.CredentialState
	}{
		{"invalid beats all", QuotaExhausted, CredInvalid, Stale, domain.StateInvalid},
		{"exhausted beats suspect", QuotaExhausted, CredSuspect, Fresh, domain.StateExhausted},
		{"suspect beats stale", QuotaHealthy, CredSuspect, Stale, domain.StateSuspect},
		{"stale beats warning", QuotaWarning, CredValid, Stale, domain.StateStale},
		{"warning", QuotaWarning, CredValid, Fresh, domain.StateWarning},
		{"notice", QuotaNotice, CredValid, Fresh, domain.StateNotice},
		{"healthy", QuotaHealthy, CredValid, Fresh, domain.StateHealthy},
		{"unknown", QuotaUnknown, CredValid, Fresh, domain.StateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Project(tc.q, tc.c, tc.f); got != tc.want {
				t.Errorf("Project = %q, want %q", got, tc.want)
			}
		})
	}
}
