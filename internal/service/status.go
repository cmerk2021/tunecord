package service

import (
	"encoding/json"
	"os"
	"time"

	"tunecord/internal/config"
)

// Status is what the running service publishes for `tunecord status`.
type Status struct {
	PID              int       `json:"pid"`
	Version          string    `json:"version"`
	StartedAt        time.Time `json:"started_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	ConfigPath       string    `json:"config_path"`
	ConfigLoaded     bool      `json:"config_loaded"`
	BusConnected     bool      `json:"bus_connected"`
	BusError         string    `json:"bus_error,omitempty"`
	DiscordConnected bool      `json:"discord_connected"`
	DiscordSocket    string    `json:"discord_socket,omitempty"`
	DiscordError     string    `json:"discord_error,omitempty"`
	Allowlist        []string  `json:"allowlist"`
	AllowlistEmpty   bool      `json:"allowlist_empty"`
	Player           string    `json:"player,omitempty"`
	PlayerIdentity   string    `json:"player_identity,omitempty"`
	PlaybackStatus   string    `json:"playback_status,omitempty"`
	Title            string    `json:"title,omitempty"`
	Artist           string    `json:"artist,omitempty"`
	Album            string    `json:"album,omitempty"`
	ArtURL           string    `json:"art_url,omitempty"`
	ProgressMode     string    `json:"progress_mode,omitempty"`
	ProgressReason   string    `json:"progress_reason,omitempty"`
	PresenceShown    bool      `json:"presence_shown"`
	Warnings         []string  `json:"warnings,omitempty"`
}

// ReadStatus reads the status file published by the running service.
func ReadStatus() (Status, error) {
	var s Status
	data, err := os.ReadFile(config.StatusPath())
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(data, &s)
	return s, err
}

func writeStatus(s Status) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(config.StatusPath(), data, 0o600)
}
