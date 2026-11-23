// Command pulsewatch watches websites and services, keeps their uptime history and serves a
// live status page.
//
//	pulsewatch -config pulsewatch.yaml
//	pulsewatch -demo            # a local demo service and a config that watches it
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Gallileee/pulsewatch/internal/check"
	"github.com/Gallileee/pulsewatch/internal/clock"
	"github.com/Gallileee/pulsewatch/internal/config"
	"github.com/Gallileee/pulsewatch/internal/demo"
	"github.com/Gallileee/pulsewatch/internal/metrics"
	"github.com/Gallileee/pulsewatch/internal/monitor"
	"github.com/Gallileee/pulsewatch/internal/notify"
	"github.com/Gallileee/pulsewatch/internal/store"
	"github.com/Gallileee/pulsewatch/internal/web"
)

// version is set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

// saveEvery is how often the history is written to the data file.
const saveEvery = time.Minute

// demoConfig watches the demo service. ${PULSEWATCH_DEMO_URL} is set to where it runs.
const demoConfig = `
title: Pulsewatch demo
listen: "127.0.0.1:8090"
incidents:
  failures_to_open: 2
  successes_to_resolve: 2
targets:
  - name: Website
    url: ${PULSEWATCH_DEMO_URL}/ok
    interval: 10s
  - name: Search API
    url: ${PULSEWATCH_DEMO_URL}/slow
    interval: 10s
    degraded_after: 1s
  - name: Payments
    url: ${PULSEWATCH_DEMO_URL}/flaky
    interval: 10s
  - name: Reports
    url: ${PULSEWATCH_DEMO_URL}/outage
    interval: 15s
    expect_body: ok
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pulsewatch:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "pulsewatch.yaml", "the config file")
	demoMode := flag.Bool("demo", false, "run a local demo service and watch it (ignores -config)")
	listen := flag.String("listen", "", "the web server's address (overrides the config)")
	showVersion := flag.Bool("version", false, "print the version")
	once := flag.Bool("once", false, "check every target once, save the history and exit (for cron or GitHub Actions)")
	siteDir := flag.String("site", "", "with -once: write the status page as static files into this directory")
	flag.Parse()
	if *showVersion {
		fmt.Println("pulsewatch", version)
		return nil
	}
	if *siteDir != "" && !*once {
		return errors.New("-site works with -once (a running Pulsewatch serves the page itself)")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	check.UserAgent = fmt.Sprintf("pulsewatch/%s (+https://github.com/Gallileee/pulsewatch)", version)

	var cfg *config.Config
	var err error
	if *demoMode {
		baseURL, stop, err := demo.Start()
		if err != nil {
			return fmt.Errorf("starting the demo service: %w", err)
		}
		defer stop()
		if err := os.Setenv("PULSEWATCH_DEMO_URL", baseURL); err != nil {
			return err
		}
		cfg, err = config.Parse([]byte(demoConfig))
		if err != nil {
			return err
		}
		cfg.Maintenance = demoMaintenance(time.Now())
		log.Info("demo service running", "url", baseURL)
	} else if cfg, err = config.Load(*configPath); err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	st := store.New()
	if cfg.DataFile != "" {
		loaded, err := store.Load(cfg.DataFile)
		switch {
		case err == nil:
			st = loaded
			log.Info("history loaded", "file", cfg.DataFile)
		case errors.Is(err, store.ErrNoFile):
			log.Info("no history yet, starting fresh", "file", cfg.DataFile)
		default:
			// Keep the unreadable file for a look later, and start fresh.
			aside := fmt.Sprintf("%s.unreadable-%d", cfg.DataFile, time.Now().Unix())
			log.Error("history unreadable, starting fresh", "error", err, "kept_as", aside)
			if err := os.Rename(cfg.DataFile, aside); err != nil {
				return fmt.Errorf("moving the unreadable history aside: %w", err)
			}
		}
	}
	var ids []string
	var described []metrics.Target
	checkers := make([]check.Checker, len(cfg.Targets))
	for i, t := range cfg.Targets {
		if checkers[i], err = check.New(t, check.Options{}); err != nil {
			return fmt.Errorf("target %q: %w", t.Name, err)
		}
		ids = append(ids, t.ID)
		described = append(described, metrics.Target{ID: t.ID, Name: t.Name, Type: t.Type})
	}
	st.Forget(ids)

	dispatcher := &notify.Dispatcher{Log: log}
	for _, a := range cfg.Alerts {
		dispatcher.Notifiers = append(dispatcher.Notifiers, notify.FromConfig(a))
	}
	m := metrics.New(version, described)
	clk := clock.Real{}
	mon := monitor.New(cfg, checkers, st, m, dispatcher, clk, log)
	site := &web.Server{
		Title: cfg.Title, Targets: cfg.Targets, Maintenance: cfg.Maintenance, Store: st, Metrics: m, Broker: mon.Broker, Clock: clk, Log: log,
	}
	if *once {
		return runOnce(mon, dispatcher, st, site, cfg.DataFile, *siteDir, log)
	}
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           site.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	serverErr := make(chan error, 1)
	go func() {
		log.Info("status page", "address", "http://"+displayAddr(cfg.Listen), "targets", len(cfg.Targets))
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()
	monitorDone := make(chan struct{})
	go func() {
		mon.Run(ctx)
		close(monitorDone)
	}()
	if cfg.DataFile != "" {
		go saveLoop(ctx, st, cfg.DataFile, log)
	}

	select {
	case <-ctx.Done():
		log.Info("stopping")
	case err := <-serverErr:
		cancel()
		<-monitorDone
		return fmt.Errorf("web server: %w", err)
	}
	shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	_ = server.Shutdown(shutdown) // the event streams end when their requests' contexts do
	<-monitorDone
	dispatcher.Wait()
	if cfg.DataFile != "" {
		if err := st.Save(cfg.DataFile); err != nil {
			return fmt.Errorf("saving the history: %w", err)
		}
	}
	return nil
}

// runOnce checks every target once, waits for the alerts, saves the history and writes the
// static site. Targets being down isn't an error: the exit code says whether Pulsewatch ran.
func runOnce(mon *monitor.Monitor, dispatcher *notify.Dispatcher, st *store.Store, site *web.Server, dataFile, siteDir string, log *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	mon.RunOnce(ctx)
	dispatcher.Wait()
	if ctx.Err() != nil {
		return errors.New("stopped before the checks finished; the history is left as it was")
	}
	if dataFile != "" {
		if err := st.Save(dataFile); err != nil {
			return fmt.Errorf("saving the history: %w", err)
		}
	}
	if siteDir != "" {
		if err := site.WriteSite(siteDir); err != nil {
			return fmt.Errorf("writing the site: %w", err)
		}
	}
	down := 0
	for _, t := range mon.Targets {
		if last, ok := st.Last(t.Config.ID); ok && last.Status == check.Down {
			down++
		}
	}
	log.Info("checked every target once", "targets", len(mon.Targets), "down", down, "site", siteDir)
	return nil
}

func saveLoop(ctx context.Context, st *store.Store, path string, log *slog.Logger) {
	ticker := time.NewTicker(saveEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := st.Save(path); err != nil {
				log.Error("saving the history", "error", err)
			}
		}
	}
}

// demoMaintenance plans two windows around the demo's start (the config file can't, its times
// are fixed): Payments is under maintenance for its first 20 minutes, so its failures open no
// incident, and a window for everything is announced for the next evening.
func demoMaintenance(now time.Time) []config.Maintenance {
	now = now.UTC().Truncate(time.Minute)
	evening := time.Date(now.Year(), now.Month(), now.Day()+1, 22, 0, 0, 0, time.UTC)
	return []config.Maintenance{
		{
			Title: "Payment provider switch", Start: config.Time{Time: now.Add(-2 * time.Minute)},
			End: config.Time{Time: now.Add(20 * time.Minute)}, Targets: []string{"Payments"}, TargetIDs: []string{"payments"},
			Note: "Card payments may fail now and then while traffic moves to the new provider.",
		},
		{
			Title: "Database upgrade", Start: config.Time{Time: evening}, End: config.Time{Time: evening.Add(90 * time.Minute)},
			Note: "Everything may be unavailable for a few minutes during the switchover.",
		},
	}
}

func displayAddr(listen string) string {
	if len(listen) > 0 && listen[0] == ':' {
		return "localhost" + listen
	}
	return listen
}
