package install

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnitContentAbsolute(t *testing.T) {
	u, err := UnitContent("/home/me/.local/bin/tunecord")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[Unit]\n",
		"Description=tunecord (MPRIS to Discord Rich Presence)\n",
		"After=graphical-session.target\n",
		"PartOf=graphical-session.target\n",
		"[Service]\n",
		"Type=simple\n",
		"ExecStart=/home/me/.local/bin/tunecord run\n",
		"Restart=on-failure\n",
		"RestartSec=5\n",
		"[Install]\n",
		"WantedBy=graphical-session.target\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit missing %q\n%s", want, u)
		}
	}
	if _, err := UnitContent("tunecord"); err == nil {
		t.Error("relative path must be rejected")
	}
	if _, err := UnitContent("~/.local/bin/tunecord"); err == nil {
		t.Error("~ path must be rejected")
	}
}

func TestUnitContentQuoting(t *testing.T) {
	u, _ := UnitContent("/home/me/My Apps/tune%cord")
	if !strings.Contains(u, `ExecStart="/home/me/My Apps/tune%%cord" run`) {
		t.Fatal(u)
	}
	u, _ = UnitContent(`/opt/a"b$c`)
	if !strings.Contains(u, `ExecStart="/opt/a\"b$$c" run`) {
		t.Fatal(u)
	}
}

func TestResolveExecutableAbsolute(t *testing.T) {
	p, err := ResolveExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p) {
		t.Fatalf("not absolute: %s", p)
	}
}

type rec struct{ calls []string }

func (r *rec) run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	return "", nil
}

func TestInstallIdempotentAndUninstall(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, "systemd", "user", "tunecord.service")
	r := &rec{}
	var out bytes.Buffer
	o := Options{ExecPath: "/home/me/.local/bin/tunecord", UnitPath: unit, Systemctl: r.run, Out: &out}
	if err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(unit)
	if err != nil || !strings.Contains(string(data), "ExecStart=/home/me/.local/bin/tunecord run") {
		t.Fatalf("%v %s", err, data)
	}
	if strings.Join(r.calls, ";") != "daemon-reload;enable --now tunecord.service" {
		t.Fatal(r.calls)
	}
	// Re-run unchanged: still reloads/enables, no restart.
	r.calls = nil
	if err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.calls, ";") != "daemon-reload;enable --now tunecord.service" {
		t.Fatal(r.calls)
	}
	// Binary moved: unit updated and service restarted.
	r.calls = nil
	o.ExecPath = "/opt/tunecord/tunecord"
	if err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(unit)
	if !strings.Contains(string(data), "ExecStart=/opt/tunecord/tunecord run") {
		t.Fatal(string(data))
	}
	if !strings.Contains(strings.Join(r.calls, ";"), "restart tunecord.service") {
		t.Fatal(r.calls)
	}
	// Uninstall removes everything.
	r.calls = nil
	if err := Uninstall(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatal("unit still present")
	}
	if strings.Join(r.calls, ";") != "disable --now tunecord.service;daemon-reload;reset-failed tunecord.service" {
		t.Fatal(r.calls)
	}
	// Uninstall again is fine.
	if err := Uninstall(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func TestInPathAndHint(t *testing.T) {
	if !InPath("/home/me/.local/bin", "/usr/bin:/home/me/.local/bin/") {
		t.Error("trailing slash")
	}
	if InPath("/home/me/.local/bin", "/usr/bin:/bin") {
		t.Error("absent")
	}
	t.Setenv("HOME", "/home/hinttest")
	if h := PathHint("/usr/bin"); !strings.Contains(h, `export PATH="$HOME/.local/bin:$PATH"`) {
		t.Error(h)
	}
	if h := PathHint("/home/hinttest/.local/bin:/usr/bin"); h != "" {
		t.Error(h)
	}
}
