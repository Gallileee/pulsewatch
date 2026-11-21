package web

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Badge is a target's status in the Shields.io endpoint format
// (https://shields.io/badges/endpoint-badge), so a README can show it:
//
//	https://img.shields.io/endpoint?url=https://example.com/badge/website.json
type Badge struct {
	SchemaVersion int    `json:"schemaVersion"`
	Label         string `json:"label"`
	Message       string `json:"message"`
	Color         string `json:"color"`
	// CacheSeconds asks Shields to fetch it again after 5 minutes (its minimum).
	CacheSeconds int `json:"cacheSeconds"`
}

// BadgeFor is the badge of one target: its status and its 7-day uptime.
func BadgeFor(t TargetView) Badge {
	b := Badge{SchemaVersion: 1, Label: "status", Message: strings.ToLower(t.StatusText), CacheSeconds: 300}
	switch t.Status {
	case "up":
		b.Color = "brightgreen"
	case "degraded":
		b.Color = "yellow"
	case "down":
		b.Color = "red"
	default:
		b.Color = "lightgrey"
	}
	if t.Uptime7d >= 0 {
		b.Message += " · " + Percent(t.Uptime7d) + " (7 d)"
	}
	return b
}

// badge serves /badge/{id}.json.
func (s *Server) badge(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".json")
	if !ok {
		http.NotFound(w, r)
		return
	}
	for _, t := range s.build().Targets {
		if t.ID == id {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			_ = json.NewEncoder(w).Encode(BadgeFor(t))
			return
		}
	}
	http.NotFound(w, r)
}
