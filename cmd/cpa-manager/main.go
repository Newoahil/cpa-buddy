// Command cpa-manager is a quota watcher for a running CLI Proxy API
// deployment.
//
// It observes credential quota across CPA-managed providers plus Ollama Cloud,
// raises Feishu alerts when a credential is exhausted, invalid or suspect, and
// offers provider-level capacity advice. It never changes credential enablement
// or policy; quota requests may trigger CPA's internal OAuth refresh.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	// Embed the timezone database so Asia/Shanghai resolves in a scratch
	// container without tzdata installed.
	_ "time/tzdata"

	"github.com/Newoahil/CPA-Manager/internal/app"
	"github.com/Newoahil/CPA-Manager/internal/calendar"
	"github.com/Newoahil/CPA-Manager/internal/collect"
	"github.com/Newoahil/CPA-Manager/internal/config"
	"github.com/Newoahil/CPA-Manager/internal/cpa"
	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/evaluate"
	"github.com/Newoahil/CPA-Manager/internal/history"
	"github.com/Newoahil/CPA-Manager/internal/logfile"
	"github.com/Newoahil/CPA-Manager/internal/notify/feishu"
	"github.com/Newoahil/CPA-Manager/internal/notify/webhook"
	"github.com/Newoahil/CPA-Manager/internal/schedule"
	"github.com/Newoahil/CPA-Manager/internal/state"
	"github.com/Newoahil/CPA-Manager/internal/web"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

// CPA reachability backoff bounds. The first probe is deliberately close to
// startup so an operator sees the outcome quickly; the ceiling keeps a
// long outage from hammering CPA.
const (
	pingBaseInterval = 30 * time.Second
	pingMaxInterval  = 15 * time.Minute
	pingAuthInterval = 5 * time.Minute
)

func main() {
	out, closeLog, logErr := logOutput()
	defer closeLog()
	log := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	if logErr != nil {
		log.Warn("log file disabled", "err", logErr)
	}

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// logOutput is stdout plus a rotating copy on the persistent volume, so past
// cycles can be inspected after the container log driver has dropped them.
// LOG_DIR=off disables the file; the size and count are bounded so the volume
// cannot fill up.
func logOutput() (io.Writer, func(), error) {
	dir := strings.TrimSpace(os.Getenv("LOG_DIR"))
	if dir == "" {
		dir = filepath.Join(filepath.Dir(envOr("STATE_PATH", "/data/state.json")), "logs")
	}
	if strings.EqualFold(dir, "off") {
		return os.Stdout, func() {}, nil
	}
	maxMB := envInt("LOG_FILE_MAX_MB", 10)
	keep := envInt("LOG_FILE_KEEP", 10)
	w, err := logfile.Open(dir, "cpa-manager.log", int64(maxMB)<<20, keep)
	if err != nil {
		return os.Stdout, func() {}, err
	}
	return io.MultiWriter(os.Stdout, w), func() { _ = w.Close() }, nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && n > 0 {
		return n
	}
	return def
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// The state volume must be writable before anything depends on durability.
	// A read-only /data makes every Save fail silently in the sense that the
	// process keeps running while re-alerting the same startup summary forever,
	// so this is a fatal, explicit check rather than a warning.
	if err := ensureStateWritable(cfg.StatePath); err != nil {
		return fmt.Errorf("state path %q is not writable: %w", cfg.StatePath, err)
	}
	log.Info("state path is writable", "path", cfg.StatePath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cpaClient := collect.NewCPAClient(cfg)

	store := state.NewFileStore(cfg.StatePath)
	engine := evaluate.New(cfg, calendar.New(cfg.Location))
	collectors, fallback := collect.Build(cfg, cpaClient, log)
	application := app.New(cfg, collectors, engine, store, log)
	// Collector failures carry this sentinel when CPA itself is the problem, so
	// app can report a control-plane outage instead of blaming credentials.
	application.SetControlPlaneError(collect.ErrControlPlane)
	if fallback != nil {
		// While the OMC source is failing and we read CPA directly, keep the
		// regular interval so the fallback does not accelerate upstream queries.
		application.SetFallbackActive(fallback.InFallback)
		// A sustained OMC outage becomes a pending omc_outage item (never an
		// immediate push); the app persists its start across restarts.
		application.SetOutageSource(fallback)
	}
	log.Info("quota source configured", "source", cfg.QuotaSource, "omc_fallback_after", cfg.OMCFallbackAfter)
	// The rate-limit cooldown watcher polls only the credential list, through
	// the same client (and its shared list flight / backoff) as the collectors.
	application.SetCooldownLister(cpaClient.ListCredentials)
	// Quota history is append-only JSONL beside the state file, split by the
	// configured zone's day boundary and pruned to QUOTA_HISTORY_RETENTION.
	hist, err := history.NewWriter(cfg.HistoryDir, cfg.Location, cfg.HistoryRetention)
	if err != nil {
		return fmt.Errorf("quota history: %w", err)
	}
	defer hist.Close()
	application.SetHistory(hist)

	notifiers, bot := buildNotifiers(cfg, application, log)
	application.SetNotifiers(notifiers)

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           web.NewHandler(application, version),
		ReadHeaderTimeout: 10 * time.Second,
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	// CPA reachability is watched, not required at boot. Exiting here would let
	// restart:unless-stopped loop on a bad management key and trip CPA's
	// five-strikes IP ban; instead the watcher runs degraded and backs off.
	spawn(&wg, func() { watchCPA(ctx, cpaClient, cfg.CPABaseURL, log) })

	spawn(&wg, func() {
		if err := application.RunPoll(ctx); err != nil {
			errCh <- err
		}
	})

	spawn(&wg, func() {
		scheduler := schedule.New(cfg.Location, cfg.SummaryTimes, application.SendSummary)
		if err := scheduler.Run(ctx); err != nil {
			errCh <- err
		}
	})

	spawn(&wg, func() {
		log.Info("status page listening", "addr", cfg.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	})

	if bot != nil {
		// The Feishu long connection is cluster-delivered, not broadcast: only
		// one replica may run it or button clicks land on an instance without
		// the latest report.
		spawn(&wg, func() {
			if err := bot.Run(ctx); err != nil && ctx.Err() == nil {
				errCh <- err
			}
		})
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errCh:
		log.Error("component failed, shutting down", "err", err)
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	wg.Wait()
	return nil
}

// ensureStateWritable is the startup write probe: it creates and removes a
// temporary file next to the state file. A brand-new named volume is owned by
// root, and a distroless nonroot process would otherwise start successfully and
// fail every Save.
func ensureStateWritable(statePath string) error {
	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".cpa-manager-write-probe-*")
	if err != nil {
		return fmt.Errorf("create temp file in %q: %w", dir, err)
	}
	name := f.Name()
	_, writeErr := f.WriteString("ok")
	closeErr := f.Close()
	removeErr := os.Remove(name)
	if writeErr != nil {
		return fmt.Errorf("write temp file: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close temp file: %w", closeErr)
	}
	if removeErr != nil {
		return fmt.Errorf("remove temp file: %w", removeErr)
	}
	return nil
}

// watchCPA probes the CPA management API with exponential backoff and never
// aborts the process. Repeated management-auth failures trip a temporary
// source-IP ban on the CPA side, so auth failures back off more conservatively
// and are logged only when the state changes.
func watchCPA(ctx context.Context, client *cpa.Client, baseURL string, log *slog.Logger) {
	interval := pingBaseInterval
	healthy := false
	lastAuth := false

	for {
		pingCtx, cancel := context.WithTimeout(ctx, client.FlightTimeout())
		err := client.Ping(pingCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}

		switch {
		case err == nil:
			if !healthy {
				log.Info("cpa management api reachable", "api_version", client.APIVersion())
			}
			healthy = true
			lastAuth = false
			interval = pingBaseInterval
		case errors.Is(err, cpa.ErrUnauthorized):
			if !lastAuth {
				log.Error("cpa management authentication failed; backing off to avoid an IP ban", "err", err)
			}
			lastAuth = true
			healthy = false
			interval = pingAuthInterval
		default:
			if healthy || !lastAuth {
				log.Warn("cpa management api unreachable; running degraded", "err", err)
			}
			healthy = false
			interval = min(interval*2, pingMaxInterval)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// buildNotifiers assembles the outbound channels that are actually configured.
//
// The generic webhook is the channel-agnostic event layer; Feishu is the first
// concrete adapter. Neither is required for the watcher to run.
func buildNotifiers(cfg config.Config, source domain.ReportSource, log *slog.Logger) ([]domain.Notifier, *feishu.Bot) {
	var notifiers []domain.Notifier

	if cfg.WebhookURL != "" {
		notifiers = append(notifiers, webhook.New(cfg.WebhookURL, nil))
		log.Info("webhook notifier enabled")
	}

	var bot *feishu.Bot
	if cfg.FeishuEnabled {
		b, err := feishu.New(cfg, source)
		if err != nil {
			// A misconfigured bot must not take the watcher down: the status
			// page and webhook still carry the signal.
			log.Error("feishu bot disabled", "err", err)
		} else {
			bot = b
			notifiers = append(notifiers, b)
			log.Info("feishu bot enabled", "chat_id", cfg.FeishuChatID)
		}
	}

	if len(notifiers) == 0 {
		log.Warn("no notification channel configured; alerts are visible only on the status page")
	}
	return notifiers, bot
}

func spawn(wg *sync.WaitGroup, fn func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		fn()
	}()
}
