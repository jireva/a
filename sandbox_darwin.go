//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const darwinSandboxExec = "/usr/bin/sandbox-exec"

const darwinSandboxProfile = `(version 1)
(allow default)
(deny file-write*)
(allow file-write*
	(literal "/dev/null")
	(subpath "/dev/fd")
	(subpath (param "WORKSPACE")))
(deny file-write* (subpath (param "STATE")))
(allow file-write*
	(subpath (param "HOME"))
	(subpath (param "TMP"))
	(subpath (param "TRANSCRIPTS")))
`

type sandboxRunner struct {
	sandboxExec string
	bash        string
	workspace   string
	home        string
	temp        string
	mounts      []sandboxMount
	subagents   *subagentSandbox
}

func newSandboxRunner() (*sandboxRunner, error) {
	workspace, err := currentWorkspace()
	if err != nil {
		return nil, err
	}
	if workspace == string(filepath.Separator) {
		return nil, errors.New("refusing to sandbox the filesystem root")
	}

	sandboxExec, err := filepath.EvalSymlinks(darwinSandboxExec)
	if err != nil {
		return nil, fmt.Errorf("find sandbox-exec: %w", err)
	}
	bash, err := hostExecutable("bash", workspace)
	if err != nil {
		return nil, err
	}

	state := filepath.Join(workspace, ".a")
	home := filepath.Join(state, "home")
	temp := filepath.Join(state, "tmp")
	if err := ensureDirectory(state, 0777); err != nil {
		return nil, err
	}
	if err := ensureDirectory(home, 0700); err != nil {
		return nil, err
	}
	if err := ensureDirectory(temp, 0700); err != nil {
		return nil, err
	}

	return &sandboxRunner{
		sandboxExec: sandboxExec,
		bash:        bash,
		workspace:   workspace,
		home:        home,
		temp:        temp,
	}, nil
}

func (r *sandboxRunner) profileArgs(transcriptDir string) []string {
	profile := darwinSandboxProfile
	var definitions []string
	for i, mount := range r.mounts {
		if mount.readOnly {
			continue
		}
		name := fmt.Sprintf("MOUNT_%d", i)
		profile += fmt.Sprintf("(allow file-write*\n\t(literal (param %q))\n\t(subpath (param %q)))\n", name, name)
		definitions = append(definitions, "-D", name+"="+mount.source)
	}
	args := []string{
		"-p", profile,
		"-D", "WORKSPACE=" + r.workspace,
		"-D", "STATE=" + filepath.Join(r.workspace, ".a"),
		"-D", "HOME=" + r.home,
		"-D", "TMP=" + r.temp,
		"-D", "TRANSCRIPTS=" + transcriptDir,
	}
	return append(args, definitions...)
}

func (r *sandboxRunner) command(command string) (shellLaunch, error) {
	transcriptDir := r.home
	environment := append(hostEnvironment(r.workspace),
		"HOME="+r.home,
		"PATH="+os.Getenv("PATH"),
		"TMPDIR="+r.temp,
		"TMP="+r.temp,
		"TEMP="+r.temp,
		"TEMPDIR="+r.temp,
	)
	if s := r.subagents; s != nil {
		var err error
		transcriptDir, err = filepath.EvalSymlinks(s.transcriptDir)
		if err != nil {
			return shellLaunch{}, fmt.Errorf("resolve subagent transcript directory: %w", err)
		}
		command = `a() { "$A_SUBAGENT_EXECUTABLE" -s -u "$A_SUBAGENT_SERVER" -m "$A_SUBAGENT_MODEL" -e "$A_SUBAGENT_EFFORT" "$@"; }` + "\n" + command
		environment = append(environment,
			"A_TRANSCRIPT_DIR="+transcriptDir,
			"A_SUBAGENT_EXECUTABLE="+s.executable,
			"A_SUBAGENT_SERVER="+s.server,
			"A_SUBAGENT_MODEL="+s.model,
			"A_SUBAGENT_EFFORT="+s.effort,
		)
	}

	args := append(r.profileArgs(transcriptDir), r.bash, "-c", command)
	cmd := exec.Command(r.sandboxExec, args...)
	cmd.Dir = r.workspace
	cmd.Env = environment
	return shellLaunch{cmd: cmd}, nil
}
