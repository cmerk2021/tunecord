// Package discord implements the Discord RPC IPC protocol over a unix
// socket, plus a reconnecting, rate-limited presence manager.
package discord

import (
	"encoding/json"
	"time"
)

// Activity types.
const (
	ActivityListening = 2
)

// Status display types (what Discord shows under the user's name).
const (
	StatusDisplayName    = 0
	StatusDisplayState   = 1
	StatusDisplayDetails = 2
)

// Activity is the subset of Discord's activity object tunecord sends.
type Activity struct {
	Type              int         `json:"type"`
	StatusDisplayType int         `json:"status_display_type"`
	Details           string      `json:"details,omitempty"`
	State             string      `json:"state,omitempty"`
	Timestamps        *Timestamps `json:"timestamps,omitempty"`
	Assets            *Assets     `json:"assets,omitempty"`
	Instance          bool        `json:"instance"`
}

// Timestamps are unix epoch milliseconds.
type Timestamps struct {
	Start int64 `json:"start,omitempty"`
	End   int64 `json:"end,omitempty"`
}

// Assets are images and their hover texts.
type Assets struct {
	LargeImage string `json:"large_image,omitempty"`
	LargeText  string `json:"large_text,omitempty"`
	SmallImage string `json:"small_image,omitempty"`
	SmallText  string `json:"small_text,omitempty"`
}

// TimestampTolerance is how much timestamps may differ while still being
// considered the same presence (avoids pushing sub-second jitter).
const TimestampTolerance = time.Second

func absMs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Same reports whether two activities would render identically (timestamps
// compared with TimestampTolerance). nil means "cleared".
func Same(a, b *Activity) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if (a.Timestamps == nil) != (b.Timestamps == nil) {
		return false
	}
	if a.Timestamps != nil {
		tol := TimestampTolerance.Milliseconds()
		if absMs(a.Timestamps.Start-b.Timestamps.Start) > tol || absMs(a.Timestamps.End-b.Timestamps.End) > tol {
			return false
		}
	}
	ac, bc := *a, *b
	ac.Timestamps, bc.Timestamps = nil, nil
	ja, _ := json.Marshal(ac)
	jb, _ := json.Marshal(bc)
	return string(ja) == string(jb)
}
