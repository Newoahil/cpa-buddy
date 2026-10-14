// Package state persists the minimum local memory the watcher needs across
// restarts: enough to avoid duplicate alerts and to recognise quota resets.
//
// It is deliberately not a history database. The file is a secret-free record
// of evaluation outcomes: it must never contain credentials, cookies, emails,
// management keys or raw upstream responses. Credential identity is the
// domain.Credential.Key, which is contractually opaque.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Newoahil/CPA-Manager/internal/domain"
)

// QuotaLevel is the quota dimension: what the numbers themselves say. It is
// independent of whether the credential is usable and of whether the data is
// fresh, so a temporary outage cannot erase a known high-usage level.
type QuotaLevel string

const (
	QuotaUnknown   QuotaLevel = "unknown"
	QuotaHealthy   QuotaLevel = "healthy"
	QuotaNotice    QuotaLevel = "notice"
	QuotaWarning   QuotaLevel = "warning"
	QuotaExhausted QuotaLevel = "exhausted"
)

// CredentialHealth is the credential-authentication dimension.
type CredentialHealth string

const (
	CredValid   CredentialHealth = "valid"
	CredInvalid CredentialHealth = "invalid"
	CredSuspect CredentialHealth = "suspect"
)

// Freshness is the data-age dimension. A stale credential may still have a
// perfectly valid auth state and a known quota level.
type Freshness string

const (
	Fresh Freshness = "fresh"
	Stale Freshness = "stale"
)

// CredentialRecord is the per-credential memory carried between cycles.
//
// It keeps three orthogonal dimensions (quota, credential, freshness) plus
// their histories. Alert deduplication is per dimension, so jitter in one
// dimension can never mask a conclusion in another.
type CredentialRecord struct {
	// Scopes preserves independent window evidence and dedup across restarts.
	// Absent in old files: legacy percentages carry unknown applicability.
	Scopes map[string]ScopeRecord `json:"scopes,omitempty"`
	// Rejections is independent of measured percentages, keyed by scope tuple.
	// Entries contain safe scope identity only, never raw errors or credentials.
	Rejections            map[string]QuotaRejection `json:"quota_rejections,omitempty"`
	CapabilityUnavailable bool                      `json:"capability_unavailable,omitempty"`
	// State is the outward projection of the three dimensions (invalid >
	// exhausted > suspect > stale > warning > notice > healthy > unknown).
	State domain.CredentialState `json:"state"`
	// Current dimension values.
	Quota      QuotaLevel       `json:"quota,omitempty"`
	Credential CredentialHealth `json:"credential,omitempty"`
	Freshness  Freshness        `json:"freshness,omitempty"`
	// LastUsedPercent is keyed by quota window name. Only reported numbers are
	// stored; a missing window means "never reported", never zero.
	LastUsedPercent map[string]float64 `json:"last_used_percent,omitempty"`
	// LastResetAt is the last reset instant we were told about, per window.
	LastResetAt         map[string]time.Time `json:"last_reset_at,omitempty"`
	LastSuccessAt       time.Time            `json:"last_success_at,omitempty"`
	ConsecutiveFailures int                  `json:"consecutive_failures,omitempty"`
	// ReachedNotice records that the quota dimension was notice or worse at
	// least once. Reset reminders are gated on it, and it is deliberately
	// separate from the projection so a stale overlay cannot hide the history.
	ReachedNotice bool `json:"reached_notice,omitempty"`
}

type ScopeRecord struct {
	Scope   domain.QuotaScope            `json:"scope"`
	ScopeID string                       `json:"scope_id,omitempty"`
	Level   QuotaLevel                   `json:"level"`
	Windows map[string]ScopeWindowRecord `json:"windows"`
}

type ScopeWindowRecord struct {
	Window        domain.QuotaWindow `json:"window"`
	Level         QuotaLevel         `json:"level"`
	ReachedNotice bool               `json:"reached_notice,omitempty"`
	ObservedAt    time.Time          `json:"observed_at"`
}

type QuotaRejection struct {
	Scope      domain.QuotaScope `json:"scope"`
	ScopeID    string            `json:"scope_id,omitempty"`
	ObservedAt time.Time         `json:"observed_at"`
}

// Project preserves stronger account/auth/freshness evidence. Limited applies
// below account notice/warning, above healthy/unknown; local evidence never
// changes the account quota dimension itself.
func (r CredentialRecord) Project() domain.CredentialState {
	q := r.Quota
	for _, rejection := range r.Rejections {
		if rejection.Scope == domain.ScopeAccount {
			q = QuotaExhausted
		}
	}
	base := Project(q, r.Credential, r.Freshness)
	if base != domain.StateHealthy && base != domain.StateUnknown {
		return base
	}
	if r.CapabilityUnavailable {
		return domain.StateUnknown
	}
	for _, rejection := range r.Rejections {
		if rejection.Scope != domain.ScopeAccount {
			return domain.StateLimited
		}
	}
	for _, scope := range r.Scopes {
		if scope.Scope == domain.ScopeAccount {
			continue
		}
		for _, w := range scope.Windows {
			if w.Window.UsedPercent != nil {
				return domain.StateLimited
			}
		}
	}
	return base
}

// maxPendingPerChannel bounds the notification outbox so a permanently broken
// channel cannot grow the state file without limit.
const maxPendingPerChannel = 50

// Project folds the three orthogonal dimensions into the outward
// domain.CredentialState shown by the API and status page.
//
// Priority: invalid > exhausted > suspect > stale > warning > notice > healthy
// > unknown. The projection is display-only; alert dedup is per dimension.
func Project(q QuotaLevel, c CredentialHealth, f Freshness) domain.CredentialState {
	switch {
	case c == CredInvalid:
		return domain.StateInvalid
	case q == QuotaExhausted:
		return domain.StateExhausted
	case c == CredSuspect:
		return domain.StateSuspect
	case f == Stale:
		return domain.StateStale
	case q == QuotaWarning:
		return domain.StateWarning
	case q == QuotaNotice:
		return domain.StateNotice
	case q == QuotaHealthy:
		return domain.StateHealthy
	default:
		return domain.StateUnknown
	}
}

// Pending status values for PendingItem.Status.
const (
	PendingOpen     = "open"
	PendingResolved = "resolved"
)

// resolvedPendingRetention is how long a resolved pending item is kept before
// it is pruned. Keeping it briefly lets the status page and a late summary show
// that a problem was fixed, without growing the file forever.
const resolvedPendingRetention = 7 * 24 * time.Hour

// PendingItem is one structured "something needs a human" record
// (docs/alert-noise-plan.md §4). It is deliberately secret-free: it carries the
// opaque credential key and the display alias, never a cookie or token.
//
// ID is stable across restarts (hash of kind + credential key or provider +
// scope) so the same ongoing problem never re-alerts after a restart.
type PendingItem struct {
	ID       string              `json:"id"`
	Kind     string              `json:"kind"`
	Provider domain.ProviderKind `json:"provider,omitempty"`
	// CredKey is empty for channel-level items.
	CredKey    string    `json:"cred_key,omitempty"`
	Alias      string    `json:"alias,omitempty"`
	Since      time.Time `json:"since"`
	LastSeen   time.Time `json:"last_seen"`
	Action     string    `json:"action,omitempty"`
	Status     string    `json:"status"`
	NotifiedAt time.Time `json:"notified_at,omitempty"`
	ResolvedAt time.Time `json:"resolved_at,omitempty"`
}

// State is the whole persisted document.
type State struct {
	Bootstrapped bool                        `json:"bootstrapped"`
	UpdatedAt    time.Time                   `json:"updated_at"`
	Credentials  map[string]CredentialRecord `json:"credentials"`
	// Pending is the delivery outbox, keyed by notifier channel name. Alerts
	// stay queued until the channel confirms delivery, so a transient notifier
	// failure cannot silently drop an alert.
	Pending map[string][]domain.Alert `json:"pending,omitempty"`
	// PendingItems are the structured open/resolved problems. Absent in old
	// files, which load as nil.
	PendingItems []PendingItem `json:"pending_items,omitempty"`
	// QuietQueue holds immediate alerts accumulated while quiet hours are
	// active, so a restart does not lose them.
	QuietQueue []domain.Alert `json:"quiet_queue,omitempty"`
}

// PendingItem returns the item with id and whether it was found.
func (s *State) PendingItem(id string) (PendingItem, bool) {
	if s == nil {
		return PendingItem{}, false
	}
	for _, it := range s.PendingItems {
		if it.ID == id {
			return it, true
		}
	}
	return PendingItem{}, false
}

// OpenPending inserts or updates a pending item, preserving Since and
// NotifiedAt of an existing open item so re-observing a problem never resets
// its age or re-alerts.
func (s *State) OpenPending(item PendingItem) {
	if s == nil || item.ID == "" {
		return
	}
	for i := range s.PendingItems {
		if s.PendingItems[i].ID != item.ID {
			continue
		}
		prev := s.PendingItems[i]
		item.Since = prev.Since
		item.NotifiedAt = prev.NotifiedAt
		item.Status = PendingOpen
		item.ResolvedAt = time.Time{}
		s.PendingItems[i] = item
		return
	}
	if item.Status == "" {
		item.Status = PendingOpen
	}
	s.PendingItems = append(s.PendingItems, item)
}

// ResolvePending marks an item resolved at now. A missing item is a no-op.
func (s *State) ResolvePending(id string, now time.Time) {
	if s == nil {
		return
	}
	for i := range s.PendingItems {
		if s.PendingItems[i].ID == id && s.PendingItems[i].Status != PendingResolved {
			s.PendingItems[i].Status = PendingResolved
			s.PendingItems[i].ResolvedAt = now
			return
		}
	}
}

// MarkPendingNotified stamps the immediate-push time on an open item.
func (s *State) MarkPendingNotified(id string, at time.Time) {
	if s == nil {
		return
	}
	for i := range s.PendingItems {
		if s.PendingItems[i].ID == id && s.PendingItems[i].Status == PendingOpen {
			s.PendingItems[i].NotifiedAt = at
			return
		}
	}
}

// PruneResolved drops resolved items kept longer than resolvedPendingRetention.
// Open items are never pruned.
func (s *State) PruneResolved(now time.Time) {
	if s == nil || len(s.PendingItems) == 0 {
		return
	}
	out := s.PendingItems[:0]
	for _, it := range s.PendingItems {
		if it.Status == PendingResolved && !it.ResolvedAt.IsZero() && now.Sub(it.ResolvedAt) > resolvedPendingRetention {
			continue
		}
		out = append(out, it)
	}
	// A nil slice stays nil; an empty non-nil slice is normalised away.
	if len(out) == 0 {
		s.PendingItems = nil
		return
	}
	s.PendingItems = out
}

// QueueQuiet appends immediate alerts accumulated during quiet hours. Duplicate
// pending-item-derived alerts are collapsed by ID so a repeated classification
// cannot grow the queue without bound.
func (s *State) QueueQuiet(alerts []domain.Alert) {
	if s == nil || len(alerts) == 0 {
		return
	}
	s.QuietQueue = append(s.QuietQueue, alerts...)
	if len(s.QuietQueue) > maxPendingPerChannel {
		s.QuietQueue = s.QuietQueue[len(s.QuietQueue)-maxPendingPerChannel:]
	}
}

// TakeQuiet returns the queued quiet-hours alerts and clears the queue.
func (s *State) TakeQuiet() []domain.Alert {
	if s == nil || len(s.QuietQueue) == 0 {
		return nil
	}
	out := append([]domain.Alert(nil), s.QuietQueue...)
	s.QuietQueue = nil
	return out
}

// QueuePending appends alerts to each named channel's outbox. A channel keeps
// at most maxPendingPerChannel alerts; the oldest are dropped first.
func (s *State) QueuePending(channels []string, alerts []domain.Alert) {
	if s == nil || len(alerts) == 0 {
		return
	}
	if s.Pending == nil {
		s.Pending = map[string][]domain.Alert{}
	}
	for _, ch := range channels {
		if ch == "" {
			continue
		}
		queue := append(s.Pending[ch], alerts...)
		if len(queue) > maxPendingPerChannel {
			queue = queue[len(queue)-maxPendingPerChannel:]
		}
		s.Pending[ch] = queue
	}
}

// PendingFor returns a copy of the queued alerts for one channel.
func (s *State) PendingFor(channel string) []domain.Alert {
	if s == nil || s.Pending == nil {
		return nil
	}
	queue := s.Pending[channel]
	if len(queue) == 0 {
		return nil
	}
	out := make([]domain.Alert, len(queue))
	copy(out, queue)
	return out
}

// ClearPending drops a channel's outbox after successful delivery.
func (s *State) ClearPending(channel string) {
	if s == nil || s.Pending == nil {
		return
	}
	delete(s.Pending, channel)
}

// Empty returns a valid, non-bootstrapped state with an initialised map.
func Empty() *State {
	return &State{Credentials: map[string]CredentialRecord{}}
}

// Store loads and saves the persisted state.
type Store interface {
	Load() (*State, error)
	Save(*State) error
}

// FileStore stores state as a single JSON file written atomically.
type FileStore struct {
	path string
}

// NewFileStore returns a Store backed by path.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// Load reads the state file.
//
// A missing or empty file is normal on first boot: it yields an empty,
// non-bootstrapped state and no error. A corrupt file also yields an empty
// state, but with an error so the caller can log it; it must never prevent the
// process from starting.
func (s *FileStore) Load() (*State, error) {
	if s == nil || s.path == "" {
		return Empty(), nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Empty(), nil
		}
		return Empty(), fmt.Errorf("read state file %q: %w", s.path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Empty(), nil
	}
	st := &State{}
	if err := json.Unmarshal(data, st); err != nil {
		return Empty(), fmt.Errorf("parse state file %q: %w", s.path, err)
	}
	if st.Credentials == nil {
		st.Credentials = map[string]CredentialRecord{}
	}
	return st, nil
}

// Save writes the state atomically: encode to a temp file in the same
// directory, fsync, then rename over the target. A crash mid-write therefore
// leaves the previous file intact instead of a half-written one.
func (s *FileStore) Save(st *State) error {
	if st == nil {
		return errors.New("state: refuse to save nil state")
	}
	if s == nil || s.path == "" {
		return errors.New("state: no path configured")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state dir %q: %w", dir, err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("create temp state in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state file %q: %w", s.path, err)
	}
	return nil
}
