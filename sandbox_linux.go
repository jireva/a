//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type sandboxRunner struct {
	bwrap     string
	bash      string
	workspace string
	home      string
	mounts    []sandboxMount
	subagents *subagentSandbox
}

func newSandboxRunner() (*sandboxRunner, error) {
	workspace, err := currentWorkspace()
	if err != nil {
		return nil, err
	}
	if workspace == string(filepath.Separator) {
		return nil, errors.New("refusing to sandbox the filesystem root")
	}

	bwrap, err := hostExecutable("bwrap", workspace)
	if err != nil {
		return nil, err
	}
	bash, err := hostExecutable("bash", workspace)
	if err != nil {
		return nil, err
	}

	home := filepath.Join(workspace, ".a", "home")
	if err := ensureDirectory(filepath.Join(workspace, ".a"), 0777); err != nil {
		return nil, err
	}
	if err := ensureDirectory(home, 0700); err != nil {
		return nil, err
	}
	return &sandboxRunner{
		bwrap:     bwrap,
		bash:      bash,
		workspace: workspace,
		home:      home,
	}, nil
}

func (r *sandboxRunner) command(command string) (shellLaunch, error) {
	if r.subagents != nil {
		command = `a() { /run/a -s -u "$A_SUBAGENT_SERVER" -m "$A_SUBAGENT_MODEL" -e "$A_SUBAGENT_EFFORT" "$@"; }` + "\n" + command
	}
	groupReader, groupWriter, err := os.Pipe()
	if err != nil {
		return shellLaunch{}, fmt.Errorf("open sandbox process-group pipe: %w", err)
	}
	group := make(chan int, 1)
	go func() {
		defer close(group)
		defer groupReader.Close()
		var info struct {
			ChildPID int `json:"child-pid"`
		}
		// --new-session makes child-pid the sandbox command's process group.
		if json.NewDecoder(groupReader).Decode(&info) == nil && info.ChildPID > 0 {
			group <- info.ChildPID
		}
	}()

	args := []string{
		"--die-with-parent",
		"--new-session",
		"--info-fd", "3",
		"--unshare-all",
		"--share-net",
		"--cap-drop", "ALL",
		"--tmpfs", "/",
		"--dir", "/run",
		"--dir", "/var",
		"--symlink", "/tmp", "/var/tmp",
	}
	for _, path := range []string{
		"/nix/store",
		filepath.Join(os.Getenv("HOME"), ".nix-profile"),
		"/etc",
		"/bin",
		"/sbin",
		"/usr",
		"/lib",
		"/lib64",
		"/run/current-system/sw",
	} {
		args = append(args, "--ro-bind-try", path, path)
	}
	const nixDaemon = "/nix/var/nix/daemon-socket"
	if _, err := os.Stat(nixDaemon); err == nil {
		args = append(args, "--ro-bind", nixDaemon, nixDaemon, "--setenv", "NIX_REMOTE", "daemon")
	}
	args = append(args,
		"--ro-bind", r.bash, r.bash,
		"--proc", "/proc",
		"--dev", "/dev",
		"--perms", "1777", "--tmpfs", "/tmp",
	)
	args = append(args,
		"--bind", r.workspace, r.workspace,
		"--ro-bind", filepath.Join(r.workspace, ".a"), filepath.Join(r.workspace, ".a"),
		"--bind", r.home, r.home,
		"--chdir", r.workspace,
		"--setenv", "HOME", r.home,
		"--setenv", "PATH", os.Getenv("PATH"),
		"--setenv", "TMPDIR", "/tmp",
		"--setenv", "TMP", "/tmp",
		"--setenv", "TEMP", "/tmp",
		"--setenv", "TEMPDIR", "/tmp",
	)
	if s := r.subagents; s != nil {
		args = append(args,
			"--bind", s.transcriptDir, s.transcriptDir,
			"--ro-bind", s.executable, "/run/a",
			"--setenv", "A_TRANSCRIPT_DIR", s.transcriptDir,
			"--setenv", "A_SUBAGENT_SERVER", s.server,
			"--setenv", "A_SUBAGENT_MODEL", s.model,
			"--setenv", "A_SUBAGENT_EFFORT", s.effort,
		)
	}
	for _, mount := range r.mounts {
		option := "--bind"
		if mount.readOnly {
			option = "--ro-bind"
		}
		args = append(args, option, mount.source, mount.target)
	}
	args = append(args, "--", r.bash, "-c", command)

	cmd := exec.Command(r.bwrap, args...)
	cmd.Dir = r.workspace
	cmd.Env = hostEnvironment(r.workspace)
	cmd.ExtraFiles = []*os.File{groupWriter}
	return shellLaunch{cmd: cmd, group: group, groupPipe: groupWriter}, nil
}
