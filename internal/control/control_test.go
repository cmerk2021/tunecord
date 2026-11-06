package control

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPidfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunecord.pid")
	if err := WritePidfile(path); err != nil {
		t.Fatal(err)
	}
	pid, err := ReadPidfile(path)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("%d %v", pid, err)
	}
	// Our own pid does not block re-writing.
	if err := WritePidfile(path); err != nil {
		t.Fatal(err)
	}
	RemovePidfile(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("pidfile not removed")
	}
	// A stale pidfile (dead pid) is overwritten.
	if err := os.WriteFile(path, []byte("999999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WritePidfile(path); err != nil {
		t.Fatalf("stale pidfile blocked: %v", err)
	}
	// A pidfile pointing to a live non-tunecord process (pid 1) is stale too.
	if err := os.WriteFile(path, []byte(strconv.Itoa(1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsTunecordProcess(1) {
		t.Skip("pid 1 looks like tunecord in this environment")
	}
	if err := WritePidfile(path); err != nil {
		t.Fatalf("foreign pid blocked: %v", err)
	}
	// Removing a pidfile owned by someone else is a no-op.
	_ = os.WriteFile(path, []byte("1"), 0o644)
	RemovePidfile(path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("foreign pidfile removed")
	}
}

func TestIsTunecordProcessDead(t *testing.T) {
	if IsTunecordProcess(999999999) || IsTunecordProcess(0) || IsTunecordProcess(-1) {
		t.Fatal("dead pid reported alive")
	}
}
