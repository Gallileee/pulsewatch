// Package notify sends alerts when an incident opens or is resolved: to a webhook as JSON, or
// as a Discord or Slack message.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Gallileee/pulsewatch/internal/config"
	"github.com/Gallileee/pulsewatch/internal/incident"
)

// Kinds of alert.
const (
	KindDown      = "down"
	KindRecovered = "recovered"
)

// Alert is one notification.
type Alert struct {
	Kind     string            `json:"kind"`
	Incident incident.Incident `json:"incident"`
	// StatusPage links to the status page, if public_url is set.
	StatusPage string `json:"status_page,omitempty"`
}

// Text is the alert as one line for a chat.
func (a Alert) Text() string {
	inc := a.Incident
	var text string
	if a.Kind == KindRecovered {
		text = fmt.Sprintf("🟢 %s is back up after %s (down since %s).",
			inc.TargetName, Duration(inc.Duration(*inc.Resolved)), inc.Started.UTC().Format("2 Jan 15:04 UTC"))
	} else {
		text = fmt.Sprintf("🔴 %s is down: %s (since %s).", inc.TargetName, inc.Cause, inc.Started.UTC().Format("2 Jan 15:04 UTC"))
	}
	if a.StatusPage != "" {
		text += " " + a.StatusPage
	}
	return text
}

// Duration writes a duration the way people say it: "45s", "12m", "3h 5m", "2d 4h".
func Duration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

// Notifier sends an alert somewhere.
type Notifier interface {
	Name() string
	Notify(ctx context.Context, a Alert) error
}

// Webhook posts the alert to a URL.
type Webhook struct {
	Label  string
	URL    string
	Format string
	Client *http.Client
	// Attempts is how many times to try (4 by default); between tries it waits 1s, 2s, 4s...
	Attempts int
	// Sleep waits between tries (tests replace it).
	Sleep func(ctx context.Context, d time.Duration) error
}

// FromConfig makes a webhook notifier.
func FromConfig(a config.Alert) *Webhook {
	label := a.Name
	if label == "" {
		label = a.Format
	}
	return &Webhook{Label: label, URL: a.URL, Format: a.Format}
}

// Name is how logs refer to it (never the URL: it may hold a secret).
func (w *Webhook) Name() string { return w.Label }

// errPermanent marks an error that another try won't fix.
type errPermanent struct{ error }

// Notify posts the alert, trying again after a network error, a 429 or a 5xx.
func (w *Webhook) Notify(ctx context.Context, a Alert) error {
	body, err := w.body(a)
	if err != nil {
		return err
	}
	attempts := w.Attempts
	if attempts == 0 {
		attempts = 4
	}
	sleep := w.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	wait := time.Second
	for try := 1; ; try++ {
		err = w.post(ctx, body)
		var permanent errPermanent
		if err == nil || errors.As(err, &permanent) || try == attempts {
			return err
		}
		if err := sleep(ctx, wait); err != nil {
			return err
		}
		wait *= 2
	}
}

func (w *Webhook) body(a Alert) ([]byte, error) {
	switch w.Format {
	case config.FormatDiscord:
		return json.Marshal(map[string]string{"content": a.Text()})
	case config.FormatSlack:
		return json.Marshal(map[string]string{"text": a.Text()})
	}
	return json.Marshal(struct {
		Alert
		Text string `json:"text"`
	}{a, a.Text()})
}

func (w *Webhook) post(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return errPermanent{errors.New("bad URL")}
	}
	req.Header.Set("Content-Type", "application/json")
	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("request failed") // the error would show the URL
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return errPermanent{fmt.Errorf("HTTP %d", resp.StatusCode)}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dispatcher sends each alert to every notifier in the background, so a slow webhook never
// holds up the checks.
type Dispatcher struct {
	Notifiers []Notifier
	Log       *slog.Logger
	// Timeout bounds one alert to one notifier, retries included (2 minutes by default).
	Timeout time.Duration
	wg      sync.WaitGroup
}

// Send starts sending the alert.
func (d *Dispatcher) Send(a Alert) {
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	for _, n := range d.Notifiers {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if err := n.Notify(ctx, a); err != nil && d.Log != nil {
				d.Log.Warn("alert not sent", "notifier", n.Name(), "incident", a.Incident.ID, "error", err)
			} else if d.Log != nil {
				d.Log.Info("alert sent", "notifier", n.Name(), "incident", a.Incident.ID, "kind", a.Kind)
			}
		}()
	}
}

// Wait waits for the alerts being sent.
func (d *Dispatcher) Wait() { d.wg.Wait() }
