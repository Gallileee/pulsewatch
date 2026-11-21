// Package demo runs a small local web service that misbehaves on purpose, so the status page
// has something to show without watching anything real: one endpoint is always fine, one is
// slow, one fails now and then, and one has an outage every few minutes.
package demo

import (
	"math/rand/v2"
	"net"
	"net/http"
	"time"
)

// OutageCycle: the outage endpoint is up for one half of each cycle and down for the other.
var OutageCycle = 6 * time.Minute

// Handler serves the demo endpoints; now tells the time (for the outage endpoint).
func Handler(now func() time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Duration(800+rand.IntN(700)) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("ok, eventually\n"))
	})
	mux.HandleFunc("GET /flaky", func(w http.ResponseWriter, _ *http.Request) {
		if rand.IntN(4) == 0 {
			http.Error(w, "flaky: failed this time", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /outage", func(w http.ResponseWriter, _ *http.Request) {
		if Down(now()) {
			http.Error(w, "planned demo outage", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// Down reports whether the outage endpoint is down at t: the second half of each cycle.
func Down(t time.Time) bool {
	return t.UnixNano()%int64(OutageCycle) >= int64(OutageCycle/2)
}

// Start serves the demo on a free local port and returns its base URL and a function that
// stops it.
func Start() (string, func(), error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: Handler(time.Now), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	return "http://" + listener.Addr().String(), func() { _ = server.Close() }, nil
}
