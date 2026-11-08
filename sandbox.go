package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type subagentSandbox struct {
	executable    string
	transcriptDir string
	server        string
	model         string
	effort        string
}

type shellLaunch struct {
	cmd       *exec.Cmd
	group     <-chan int
	groupPipe *os.File
}

func (l shellLaunch) closeGroupPipe() {
	if l.groupPipe != nil {
		_ = l.groupPipe.Close()
	}
}

func hostShellCommand(command string) (shellLaunch, error) {
	workspace, err := currentWorkspace()
	if err != nil {
		return shellLaunch{}, err
	}
	bash, err := hostExecutable("bash", workspace)
	if err != nil {
		return shellLaunch{}, err
	}
	cmd := exec.Command(bash, "-c", command)
	cmd.Env = hostEnvironment(workspace)
	return shellLaunch{cmd: cmd}, nil
}

func (r *sandboxRunner) run(command string) (string, error) {
	launch, err := r.command(command)
	if err != nil {
		return "", err
	}
	var commands shellCommands
	defer commands.shutdown()
	return commands.run("shell", false, launch, "run sandboxed shell")
}

func (r *sandboxRunner) enableSubagents(transcriptDir, server, model, effort string) error {
	if err := ensureDirectory(transcriptDir, 0777); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find current executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	r.subagents = &subagentSandbox{
		executable:    executable,
		transcriptDir: transcriptDir,
		server:        server,
		model:         model,
		effort:        effort,
	}
	return nil
}

func currentWorkspace() (string, error) {
	workspace, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve current directory: %w", err)
	}
	return workspace, nil
}

func ensureDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, mode); err != nil {
			return fmt.Errorf("create directory %q: %w", path, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect directory %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	return nil
}

func hostEnvironment(workspace string) []string {
	environment := os.Environ()
	filtered := environment[:0]
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		if name == "BASH_ENV" || name == "ENV" || name == "BASHOPTS" || name == "SHELLOPTS" ||
			strings.HasPrefix(name, "BASH_FUNC_") || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
			continue
		}
		if name == "PATH" {
			entry = "PATH=" + hostPath(value, workspace)
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func hostPath(path, workspace string) string {
	var safe []string
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) || pathInside(workspace, filepath.Clean(dir)) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil || pathInside(workspace, resolved) {
			continue
		}
		safe = append(safe, dir)
	}
	return strings.Join(safe, string(os.PathListSeparator))
}

func pathInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func hostExecutable(name, workspace string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("find %s: %w", name, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", name, err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", name, err)
	}
	if pathInside(workspace, path) {
		return "", fmt.Errorf("refusing to run host %s from inside the workspace", name)
	}
	return path, nil
}

type sandboxMount struct {
	source   string
	target   string
	readOnly bool
}

func prepareSandboxMounts(readOnly, readWrite, homePaths []string) ([]sandboxMount, error) {
	if len(readOnly)+len(readWrite)+len(homePaths) == 0 {
		return nil, nil
	}
	workspace, err := currentWorkspace()
	if err != nil {
		return nil, err
	}

	var mounts []sandboxMount
	byTarget := make(map[string]int)
	add := func(option, path, target string, ro bool) error {
		source, err := filepath.EvalSymlinks(target)
		if err != nil {
			return fmt.Errorf("-%s %q: %w", option, path, err)
		}
		if option != "~" && pathInside(workspace, source) {
			return fmt.Errorf("-%s %q is inside the workspace", option, path)
		}
		info, err := os.Stat(source)
		if err != nil {
			return fmt.Errorf("-%s %q: %w", option, path, err)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("-%s %q is not a regular file or directory", option, path)
		}
		if i, ok := byTarget[target]; ok {
			if !ro {
				mounts[i].readOnly = false
			}
			return nil
		}
		byTarget[target] = len(mounts)
		mounts = append(mounts, sandboxMount{source, target, ro})
		return nil
	}
	for _, set := range []struct {
		option   string
		paths    []string
		readOnly bool
	}{{"r", readOnly, true}, {"rw", readWrite, false}} {
		for _, path := range set.paths {
			target := path
			if !filepath.IsAbs(target) {
				target = filepath.Join(workspace, target)
			}
			target = filepath.Clean(target)
			if pathInside(workspace, target) {
				return nil, fmt.Errorf("-%s %q is inside the workspace", set.option, path)
			}
			if err := add(set.option, path, target, set.readOnly); err != nil {
				return nil, err
			}
		}
	}

	if len(homePaths) != 0 {
		hostHome, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("find host home: %w", err)
		}
		hostHome, err = filepath.Abs(hostHome)
		if err != nil {
			return nil, fmt.Errorf("resolve host home: %w", err)
		}
		sandboxHome := filepath.Join(workspace, ".a", "home")
		if err := ensureDirectory(filepath.Dir(sandboxHome), 0777); err != nil {
			return nil, err
		}
		if err := ensureDirectory(sandboxHome, 0700); err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(sandboxHome)
		if err != nil {
			return nil, fmt.Errorf("open sandbox home: %w", err)
		}
		defer root.Close()

		for _, path := range homePaths {
			relative := filepath.Clean(path)
			if relative == "." || !filepath.IsLocal(path) {
				return nil, fmt.Errorf("-~ %q must name a descendant of the host home", path)
			}
			target := filepath.Join(hostHome, relative)
			if err := add("~", path, target, false); err != nil {
				return nil, err
			}
			if err := root.MkdirAll(filepath.Dir(relative), 0700); err != nil {
				return nil, fmt.Errorf("create sandbox home parent for %q: %w", relative, err)
			}
			link, err := root.Readlink(relative)
			if errors.Is(err, os.ErrNotExist) {
				if err := root.Symlink(target, relative); err != nil {
					return nil, fmt.Errorf("link sandbox home path %q: %w", relative, err)
				}
				continue
			}
			if err != nil || link != target {
				return nil, fmt.Errorf("sandbox home path %q already exists", relative)
			}
		}
	}
	return mounts, nil
}
