//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDarwinSandbox(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	secret := filepath.Join(parent, "secret")
	writable := filepath.Join(parent, "writable")
	for _, dir := range []string{workspace, writable} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(secret, []byte("outside-readable\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WRITABLE", writable)
	t.Chdir(workspace)

	runner, err := newSandboxRunner()
	if err != nil {
		t.Fatal(err)
	}
	runner.mounts = []sandboxMount{{source: writable, target: writable}}
	got, err := runner.run(`cat ../secret; printf writable > result; printf shared > "$WRITABLE/result"; if printf changed > ../secret; then exit 9; fi`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "outside-readable\n") || !strings.HasSuffix(got, "exit status: 0") {
		t.Fatalf("result = %q", got)
	}
	if data, err := os.ReadFile(filepath.Join(workspace, "result")); err != nil || string(data) != "writable" {
		t.Fatalf("workspace result = %q, %v", data, err)
	}
	if data, err := os.ReadFile(secret); err != nil || string(data) != "outside-readable\n" {
		t.Fatalf("outside file = %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(writable, "result")); err != nil || string(data) != "shared" {
		t.Fatalf("shared result = %q, %v", data, err)
	}
}
