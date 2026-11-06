// Package control manages the pidfile, systemctl calls and service reloads.
package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tunecord/internal/config"
)

// UnitName is the systemd user unit name.
const UnitName = "tunecord.service"

// ErrAlreadyRunning is returned when another tunecord instance holds the pidfile.
var ErrAlreadyRunning = errors.New("tunecord is already running")

// processAlive reports whether pid exists (signal 0).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// IsTunecordProcess reports whether pid is a live tunecord process
// (guards against stale pidfiles whose pid was reused).
func IsTunecordProcess(pid int) bool {
	if !processAlive(pid) {
		return false
	}
	proc := filepath.Join("/proc", strconv.Itoa(pid))
	if exe, err := os.Readlink(filepath.Join(proc, "exe")); err == nil {
		if strings.Contains(filepath.Base(strings.TrimSuffix(exe, " (deleted)")), config.AppName) {
			return true
		}
	}
	if comm, err := os.ReadFile(filepath.Join(proc, "comm")); err == nil {
		if strings.Contains(strings.TrimSpace(string(comm)), config.AppName) {
			return true
		}
	}
	if cmdline, err := os.ReadFile(filepath.Join(proc, "cmdline")); err == nil {
		argv0 := string(bytes.SplitN(cmdline, []byte{0}, 2)[0])
		if strings.Contains(filepath.Base(argv0), config.AppName) {
			return true
		}
	}
	return false
}

// ReadPidfile returns the pid stored at path.
func ReadPidfile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid pidfile %s", path)
	}
	return pid, nil
}

// RunningPID returns the pid of a running tunecord service, or 0.
func RunningPID() int {
	pid, err := ReadPidfile(config.PidfilePath())
	if err != nil || pid == os.Getpid() || !IsTunecordProcess(pid) {
		return 0
	}
	return pid
}

// WritePidfile writes our pid, refusing if another live tunecord owns it.
func WritePidfile(path string) error {
	if pid, err := ReadPidfile(path); err == nil && pid != os.Getpid() && IsTunecordProcess(pid) {
		return fmt.Errorf("%w (pid %d, pidfile %s)", ErrAlreadyRunning, pid, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return config.WriteFileAtomic(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
}

// RemovePidfile removes the pidfile if it still contains our pid.
func RemovePidfile(path string) {
	if pid, err := ReadPidfile(path); err == nil && pid == os.Getpid() {
		_ = os.Remove(path)
	}
}

// HaveSystemctl reports whether systemctl is available.
func HaveSystemctl() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// Systemctl runs `systemctl --user <args>` and returns combined output.
func Systemctl(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s != "" {
			return s, fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, s)
		}
		return s, fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return s, nil
}

// UnitActive reports whether the tunecord user unit is active.
func UnitActive(ctx context.Context) bool {
	if !HaveSystemctl() {
		return false
	}
	_, err := Systemctl(ctx, "is-active", "--quiet", UnitName)
	return err == nil
}

// UnitState returns the unit's ActiveState ("active", "inactive", ...), or "" if unknown.
func UnitState(ctx context.Context) string {
	if !HaveSystemctl() {
		return ""
	}
	out, _ := Systemctl(ctx, "show", "--property=ActiveState", "--value", UnitName)
	return strings.TrimSpace(out)
}

// Reload makes a running service pick up a new config: restart the
// systemd unit if active, otherwise SIGHUP a pidfile-tracked process.
func Reload(ctx context.Context) (string, error) {
	if UnitActive(ctx) {
		if _, err := Systemctl(ctx, "restart", UnitName); err != nil {
			return "", err
		}
		return "Restarted " + UnitName + " to apply the new configuration.", nil
	}
	if pid := RunningPID(); pid != 0 {
		if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
			return "", fmt.Errorf("send SIGHUP to tunecord (pid %d): %w", pid, err)
		}
		return fmt.Sprintf("Sent SIGHUP to the running tunecord (pid %d); it reloaded the configuration.", pid), nil
	}
	return "The tunecord service is not running. Start it with 'tunecord install --user' (systemd) or 'tunecord run' (foreground).", nil
}
