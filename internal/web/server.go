// Package web serves the status page, its JSON, live updates (server-sent events) and the
// Prometheus metrics.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Gallileee/pulsewatch/internal/clock"
	"github.com/Gallileee/pulsewatch/internal/config"
	"github.com/Gallileee/pulsewatch/internal/metrics"
	"github.com/Gallileee/pulsewatch/internal/monitor"
	"github.com/Gallileee/pulsewatch/internal/notify"
	"github.com/Gallileee/pulsewatch/internal/store"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// Heartbeat is how often an idle event stream gets a comment, so proxies keep it open.
var Heartbeat = 25 * time.Second

// Server is the web side.
type Server struct {
	Title   string
	Targets []config.Target
	// Maintenance is the planned downtime shown on the page.
	Maintenance []config.Maintenance
	Store       *store.Store
	Metrics     *metrics.Metrics
	Broker      *monitor.Broker
	Clock       clock.Clock
	Log         *slog.Logger
	tmpl        *template.Template
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	s.parse()
	static, _ := fs.Sub(staticFiles, "static")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page("index.html"))
	mux.HandleFunc("GET /partials/live", s.page("live"))
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("GET /badge/{file}", s.badge)
	mux.HandleFunc("GET /maintenance.ics", s.calendar)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	return securityHeaders(mux)
}

// WriteSite writes the status page as static files into dir: index.html, status.json,
// maintenance.ics, the badges and the static/ assets, for hosting without Pulsewatch running
// (GitHub Pages, any web server).
func (s *Server) WriteSite(dir string) error {
	s.parse()
	page := s.build()
	page.Static = true
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0o755); err != nil {
		return err
	}
	var html bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&html, "index.html", page); err != nil {
		return err
	}
	data, err := json.MarshalIndent(page, "", "  ")
	if err != nil {
		return err
	}
	files := map[string][]byte{
		"index.html":      html.Bytes(),
		"status.json":     append(data, '\n'),
		"maintenance.ics": Calendar(s.Title, s.Maintenance, s.Targets, page.Generated),
	}
	if err := os.MkdirAll(filepath.Join(dir, "badge"), 0o755); err != nil {
		return err
	}
	for _, t := range page.Targets {
		badge, err := json.Marshal(BadgeFor(t))
		if err != nil {
			return err
		}
		files[filepath.Join("badge", t.ID+".json")] = append(badge, '\n')
	}
	assets, _ := fs.Sub(staticFiles, "static")
	if err := fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := fs.ReadFile(assets, path)
		files[filepath.Join("static", path)] = content
		return err
	}); err != nil {
		return err
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) parse() {
	if s.tmpl != nil {
		return
	}
	s.tmpl = template.Must(template.New("").Funcs(template.FuncMap{
		"percent":  Percent,
		"duration": notify.Duration,
		"iso":      func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"clock":    func(t time.Time) string { return t.UTC().Format("2 Jan 2006, 15:04 UTC") },
		"ms":       Milliseconds,
	}).ParseFS(templateFiles, "templates/*.html"))
}

func (s *Server) build() Page {
	return Build(s.Title, s.Targets, s.Maintenance, s.Store, s.Clock.Now())
}

func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.tmpl.ExecuteTemplate(w, name, s.build()); err != nil && s.Log != nil {
			s.Log.Error("rendering the page", "template", name, "error", err)
		}
	}
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*") // public data: other pages may embed it
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s.build())
}

// calendar serves the maintenance windows for calendar apps to subscribe to.
func (s *Server) calendar(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(Calendar(s.Title, s.Maintenance, s.Targets, s.Clock.Now()))
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = s.Metrics.WriteTo(w)
}

// events streams an event for every check, as server-sent events.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: don't buffer the stream
	events, cancel := s.Broker.Subscribe()
	defer cancel()
	// Tell the browser to wait 5 s before reconnecting, and that the stream is open.
	if _, err := fmt.Fprint(w, "retry: 5000\n: connected\n\n"); err != nil || rc.Flush() != nil {
		return
	}
	heartbeat := time.NewTicker(Heartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-events:
			data, _ := json.Marshal(e)
			if _, err := fmt.Fprintf(w, "event: check\ndata: %s\n\n", data); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		}
		if rc.Flush() != nil {
			return
		}
	}
}

// Milliseconds writes a latency: "0.4 ms", "12 ms", "1.36 s".
func Milliseconds(ms float64) string {
	switch {
	case ms < 0.1:
		return "< 0.1 ms"
	case ms < 10:
		return fmt.Sprintf("%.1f ms", ms)
	case ms < 1000:
		return fmt.Sprintf("%.0f ms", ms)
	}
	return fmt.Sprintf("%.2f s", ms/1000)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
