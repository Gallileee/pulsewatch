package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gallileee/pulsewatch/internal/config"
	"github.com/Gallileee/pulsewatch/internal/incident"
)

var started = time.Date(2026, 11, 21, 14, 3, 0, 0, time.UTC)

func downAlert() Alert {
	return Alert{Kind: KindDown, Incident: incident.Incident{
		ID: "web-1", TargetID: "web", TargetName: "Website", Started: started, Cause: "HTTP 502 Bad Gateway",
	}}
}

func TestText(t *testing.T) {
	if got := downAlert().Text(); got != "🔴 Website is down: HTTP 502 Bad Gateway (since 21 Nov 14:03 UTC)." {
		t.Errorf("down: %q", got)
	}
	a := downAlert()
	a.Kind = KindRecovered
	resolved := started.Add(3*time.Hour + 5*time.Minute)
	a.Incident.Resolved = &resolved
	a.StatusPage = "https://status.example.com"
	if got := a.Text(); got != "🟢 Website is back up after 3h 5m (down since 21 Nov 14:03 UTC). https://status.example.com" {
		t.Errorf("recovered: %q", got)
	}
}

func TestDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Second:                "45s",
		12*time.Minute + 30*time.Second: "12m",
		3*time.Hour + 5*time.Minute:     "3h 5m",
		52 * time.Hour:                  "2d 4h",
	} {
		if got := Duration(d); got != want {
			t.Errorf("%v: %q", d, got)
		}
	}
}

type received struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func server(t *testing.T, codes ...int) (*httptest.Server, *received, *atomic.Int32) {
	t.Helper()
	got := &received{}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(data, &body)
		got.mu.Lock()
		got.bodies = append(got.bodies, body)
		got.mu.Unlock()
		code := http.StatusNoContent
		if n <= len(codes) {
			code = codes[n-1]
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv, got, &calls
}

func noSleep(waits *[]time.Duration) func(context.Context, time.Duration) error {
	return func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
}

func TestFormats(t *testing.T) {
	srv, got, _ := server(t)
	for _, format := range []string{config.FormatDiscord, config.FormatSlack, config.FormatWebhook} {
		w := &Webhook{URL: srv.URL, Format: format}
		if err := w.Notify(context.Background(), downAlert()); err != nil {
			t.Fatal(err)
		}
	}
	if got.bodies[0]["content"] != downAlert().Text() || len(got.bodies[0]) != 1 {
		t.Errorf("discord %v", got.bodies[0])
	}
	if got.bodies[1]["text"] != downAlert().Text() || len(got.bodies[1]) != 1 {
		t.Errorf("slack %v", got.bodies[1])
	}
	full := got.bodies[2]
	inc, _ := full["incident"].(map[string]any)
	if full["kind"] != "down" || full["text"] != downAlert().Text() || inc["target"] != "web" || inc["cause"] != "HTTP 502 Bad Gateway" {
		t.Errorf("webhook %v", full)
	}
}

func TestRetriesServerErrors(t *testing.T) {
	srv, _, calls := server(t, 503, 429, 500)
	var waits []time.Duration
	w := &Webhook{URL: srv.URL, Sleep: noSleep(&waits)}
	if err := w.Notify(context.Background(), downAlert()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || len(waits) != 3 || waits[0] != time.Second || waits[2] != 4*time.Second {
		t.Errorf("%d calls, waits %v", calls.Load(), waits)
	}
}

func TestGivesUp(t *testing.T) {
	srv, _, calls := server(t, 500, 500, 500, 500, 500)
	var waits []time.Duration
	w := &Webhook{URL: srv.URL, Attempts: 3, Sleep: noSleep(&waits)}
	if err := w.Notify(context.Background(), downAlert()); err == nil || err.Error() != "HTTP 500" {
		t.Errorf("err %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("%d calls", calls.Load())
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	srv, _, calls := server(t, 404)
	var waits []time.Duration
	w := &Webhook{URL: srv.URL, Sleep: noSleep(&waits)}
	if err := w.Notify(context.Background(), downAlert()); err == nil || calls.Load() != 1 {
		t.Errorf("err %v, %d calls", err, calls.Load())
	}
}

func TestNetworkErrorsHideTheURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/secret-token"
	srv.Close()
	var waits []time.Duration
	w := &Webhook{URL: url, Attempts: 2, Sleep: noSleep(&waits)}
	err := w.Notify(context.Background(), downAlert())
	if err == nil || strings.Contains(err.Error(), "secret-token") || len(waits) != 1 {
		t.Errorf("err %v, waits %v", err, waits)
	}
}

func TestStopsWhenCancelled(t *testing.T) {
	srv, _, calls := server(t, 500, 500)
	ctx, cancel := context.WithCancel(context.Background())
	w := &Webhook{URL: srv.URL, Sleep: func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}}
	if err := w.Notify(ctx, downAlert()); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Errorf("err %v, %d calls", err, calls.Load())
	}
}

type recorder struct {
	mu     sync.Mutex
	alerts []Alert
	fail   bool
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) Notify(_ context.Context, a Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, a)
	if r.fail {
		return errors.New("nope")
	}
	return nil
}

func TestDispatcherSendsToEveryNotifier(t *testing.T) {
	a, b := &recorder{}, &recorder{fail: true}
	d := &Dispatcher{Notifiers: []Notifier{a, b}}
	d.Send(downAlert())
	d.Wait()
	if len(a.alerts) != 1 || len(b.alerts) != 1 {
		t.Errorf("%d %d", len(a.alerts), len(b.alerts))
	}
}

func TestFromConfig(t *testing.T) {
	w := FromConfig(config.Alert{Format: config.FormatSlack, URL: "https://hooks.test/x"})
	if w.Name() != "slack" || w.Format != config.FormatSlack {
		t.Errorf("%+v", w)
	}
	if FromConfig(config.Alert{Name: "team", Format: config.FormatSlack}).Name() != "team" {
		t.Error("name")
	}
}
