package demo

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDown(t *testing.T) {
	start := time.Unix(0, 0)
	if Down(start) || Down(start.Add(OutageCycle/2-time.Second)) {
		t.Error("down in the first half")
	}
	if !Down(start.Add(OutageCycle/2)) || !Down(start.Add(OutageCycle-time.Second)) {
		t.Error("up in the second half")
	}
	if Down(start.Add(OutageCycle)) {
		t.Error("down at the next cycle")
	}
}

func TestEndpoints(t *testing.T) {
	at := time.Unix(0, 0)
	srv := httptest.NewServer(Handler(func() time.Time { return at }))
	defer srv.Close()
	status := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if status("/ok") != 200 || status("/outage") != 200 {
		t.Error("not up")
	}
	at = at.Add(OutageCycle / 2)
	if status("/outage") != http.StatusBadGateway {
		t.Error("the outage endpoint is up during its outage")
	}
	seen := map[int]bool{}
	for range 60 {
		seen[status("/flaky")] = true
	}
	if !seen[200] || !seen[503] {
		t.Errorf("flaky answered %v", seen)
	}
}

func TestStart(t *testing.T) {
	base, stop, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	resp, err := http.Get(base + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("%d", resp.StatusCode)
	}
}
