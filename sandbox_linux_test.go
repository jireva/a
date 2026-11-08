//go:build linux

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShell(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	userDirectory := filepath.Join(parent, "home", ".a")
	if err := os.MkdirAll(userDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(userDirectory, "shared")
	if err := os.WriteFile(shared, []byte("shared\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Dir(userDirectory))
	t.Setenv("TEST_USER_DIRECTORY", userDirectory)
	t.Chdir(workspace)
	runner, err := newSandboxRunner()
	if err != nil {
		t.Fatal(err)
	}
	got, err := runner.run("printf 'out\\n'; printf 'err\\n' >&2; printf '\\377x'; exit 7")
	if err != nil {
		t.Fatal(err)
	}
	if want := "out\nerr\n�x\nexit status: 7"; got != want {
		t.Fatalf("result = %q, want %q", got, want)
	}
	got, err = runner.run(`test ! -e ../secret && test ! -e "$TEST_USER_DIRECTORY/shared" && printf writable > result && printf persistent > "$HOME/value" && cat result`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "writable\nexit status: 0" {
		t.Fatalf("result = %q", got)
	}
	runner, err = newSandboxRunner()
	if err != nil {
		t.Fatal(err)
	}
	got, err = runner.run(`cat "$HOME/value"`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "persistent\nexit status: 0" {
		t.Fatalf("result = %q", got)
	}
}

func TestSandboxMounts(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	hostHome := filepath.Join(parent, "host-home")
	readOnly := filepath.Join(parent, "read-only")
	readWrite := filepath.Join(parent, "read-write")
	config := filepath.Join(parent, "gh-config")
	for _, dir := range []string{workspace, readWrite, filepath.Join(hostHome, ".config"), config} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(readOnly, []byte("reference\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "config.yml"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(config, filepath.Join(hostHome, ".config", "gh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", hostHome)
	t.Setenv("READ_ONLY", readOnly)
	t.Setenv("READ_WRITE", readWrite)
	t.Chdir(workspace)

	var stderr bytes.Buffer
	a := app{stdin: strings.NewReader(""), stdout: io.Discard, stderr: &stderr}
	args := []string{"-r", readOnly, "-r", readWrite, "-rw", readWrite, "-~", ".config/gh"}
	if code := a.run(args); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if len(a.mounts) != 3 || !a.mounts[0].readOnly || a.mounts[1].readOnly || a.mounts[2].readOnly {
		t.Fatalf("mounts = %#v", a.mounts)
	}
	runner, err := newSandboxRunner()
	if err != nil {
		t.Fatal(err)
	}
	runner.mounts = a.mounts
	got, err := runner.run(`
cat "$READ_ONLY"
if { printf changed > "$READ_ONLY"; } 2>/dev/null; then exit 8; fi
printf writable > "$READ_WRITE/result"
printf updated > "$HOME/.config/gh/config.yml"
`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "reference\nexit status: 0" {
		t.Fatalf("result = %q", got)
	}
	if data, err := os.ReadFile(readOnly); err != nil || string(data) != "reference\n" {
		t.Fatalf("read-only file = %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(readWrite, "result")); err != nil || string(data) != "writable" {
		t.Fatalf("read-write file = %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(config, "config.yml")); err != nil || string(data) != "updated" {
		t.Fatalf("home file = %q, %v", data, err)
	}
	if _, err := prepareSandboxMounts(nil, nil, []string{".config/gh"}); err != nil {
		t.Fatalf("reuse home link: %v", err)
	}
}

func TestSandboxGraceTargetsCommandGroup(t *testing.T) {
	oldRunTime, oldGraceTime := shellRunTime, shellGraceTime
	shellRunTime = 20 * time.Millisecond
	shellGraceTime = 2 * time.Second
	t.Cleanup(func() {
		shellRunTime = oldRunTime
		shellGraceTime = oldGraceTime
	})

	t.Chdir(t.TempDir())
	runner, err := newSandboxRunner()
	if err != nil {
		t.Fatal(err)
	}
	launch, err := runner.command(`trap 'printf handled > term-marker; exit 0' TERM; printf ready; while :; do :; done`)
	if err != nil {
		t.Fatal(err)
	}
	var commands shellCommands
	t.Cleanup(commands.shutdown)
	got, err := commands.run("sandbox", false, launch, "run sandboxed helper")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "command still running: sandbox") {
		t.Fatalf("initial result = %q", got)
	}
	ready := strings.Contains(got, "ready")
	deadline := time.Now().Add(time.Second)
	for !ready && time.Now().Before(deadline) {
		got, err = commands.read("sandbox")
		if err != nil {
			t.Fatal(err)
		}
		ready = strings.Contains(got, "ready")
		if !ready {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !ready {
		t.Fatal("sandbox command did not become ready")
	}
	commands.endTurn()
	deadline = time.Now().Add(time.Second)
	var marker []byte
	for {
		marker, _ = os.ReadFile("term-marker")
		if string(marker) == "handled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox command did not handle SIGTERM; marker = %q", marker)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
