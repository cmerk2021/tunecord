package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// AppName is used for directories, files and the systemd unit.
const AppName = "tunecord"

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}

// ConfigDir returns $XDG_CONFIG_HOME/tunecord or ~/.config/tunecord.
func ConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, AppName)
	}
	return filepath.Join(homeDir(), ".config", AppName)
}

// ConfigPath returns the default config file path.
func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.toml")
}

// CacheDir returns $XDG_CACHE_HOME/tunecord or ~/.cache/tunecord.
func CacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, AppName)
	}
	return filepath.Join(homeDir(), ".cache", AppName)
}

// RuntimeDir returns $XDG_RUNTIME_DIR, falling back to /run/user/<uid>
// and finally to a per-user directory in the system temp dir.
func RuntimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" && filepath.IsAbs(d) {
		return d
	}
	candidate := fmt.Sprintf("/run/user/%d", os.Getuid())
	if st, err := os.Stat(candidate); err == nil && st.IsDir() {
		return candidate
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", AppName, os.Getuid()))
}

// PidfilePath returns $XDG_RUNTIME_DIR/tunecord.pid.
func PidfilePath() string {
	return filepath.Join(RuntimeDir(), AppName+".pid")
}

// StatusPath returns the file the running service publishes its state to.
func StatusPath() string {
	return filepath.Join(RuntimeDir(), AppName+".status.json")
}

// SystemdUserDir returns $XDG_CONFIG_HOME/systemd/user or ~/.config/systemd/user.
func SystemdUserDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "systemd", "user")
	}
	return filepath.Join(homeDir(), ".config", "systemd", "user")
}
