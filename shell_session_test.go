package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestShellOutputLimitKeepsNewestBytes(t *testing.T) {
	process := &shellProcess{full: make(chan struct{})}
	output := append(bytes.Repeat([]byte("a"), shellBufferSize), []byte("newest")...)
	if n, err := process.Write(output); err != nil || n != len(output) {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	select {
	case <-process.full:
	default:
		t.Fatal("output limit did not signal")
	}

	got, exited, err := process.takeResult("command")
	if err != nil || exited {
		t.Fatalf("takeResult() = exited %v, error %v", exited, err)
	}
	wantPrefix := "output discarded: 6 bytes\n"
	wantOutput := strings.Repeat("a", shellBufferSize-6) + "newest\ncommand still running: command"
	if got != wantPrefix+wantOutput {
		t.Fatalf("result length = %d, want %d", len(got), len(wantPrefix)+len(wantOutput))
	}
	got, _, _ = process.takeResult("command")
	if got != "command still running: command" {
		t.Fatalf("second result = %q", got)
	}
}

func TestShellWriteReadAndEOF(t *testing.T) {
	oldRunTime := shellRunTime
	shellRunTime = 20 * time.Millisecond
	t.Cleanup(func() { shellRunTime = oldRunTime })

	var commands shellCommands
	t.Cleanup(commands.shutdown)
	got, err := commands.run("echo", true, shellLaunch{cmd: shellHelperCommand("echo")}, "run helper")
	if err != nil {
		t.Fatal(err)
	}
	if got != "command still running: echo" {
		t.Fatalf("initial result = %q", got)
	}
	process := commands.find("echo")
	if process == nil {
		t.Fatal("running command was not retained")
	}

	inputs := []string{"left", "right"}
	errs := make(chan error, len(inputs))
	for _, input := range inputs {
		go func() {
			_, err := process.enqueue(input, false)
			errs <- err
		}()
	}
	for range inputs {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got, err := process.enqueue("", true); err != nil || got != "stdin close queued" {
		t.Fatalf("close enqueue = %q, %v", got, err)
	}
	select {
	case <-process.done:
	case <-time.After(2 * time.Second):
		t.Fatal("command did not exit after EOF")
	}

	got, err = commands.read("echo")
	if err != nil {
		t.Fatal(err)
	}
	if got != "leftright\nexit status: 0" && got != "rightleft\nexit status: 0" {
		t.Fatalf("final result = %q", got)
	}
	if _, err := commands.read("echo"); err == nil {
		t.Fatal("finished command was not forgotten")
	}
}

func TestShellWait(t *testing.T) {
	newProcess := func(output string) *shellProcess {
		return &shellProcess{done: make(chan struct{}), output: []byte(output)}
	}
	markComplete := func(process *shellProcess) {
		process.mu.Lock()
		process.exited = true
		process.mu.Unlock()
	}

	first := newProcess("alpha\n")
	second := newProcess("beta\n")
	pending := newProcess("gamma\n")
	commands := shellCommands{active: map[string]*shellProcess{
		"first": first, "second": second, "pending": pending,
	}}
	type waitResult struct {
		output string
		err    error
	}
	waiting := make(chan waitResult, 1)
	go func() {
		output, err := commands.wait([]string{"first", "second", "pending"})
		waiting <- waitResult{output, err}
	}()
	select {
	case result := <-waiting:
		t.Fatalf("wait returned before a command exited: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}

	markComplete(first)
	markComplete(second)
	close(first.done)
	var result waitResult
	select {
	case result = <-waiting:
	case <-time.After(time.Second):
		t.Fatal("wait did not return after a command exited")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	for _, want := range []string{"alpha", "beta"} {
		if !strings.Contains(result.output, want) {
			t.Fatalf("wait result = %q, missing %q", result.output, want)
		}
	}
	if strings.Contains(result.output, "gamma") {
		t.Fatalf("wait returned pending command: %q", result.output)
	}
	if commands.find("first") != nil || commands.find("second") != nil || commands.find("pending") != pending {
		t.Fatal("wait did not retire completed commands and retain the pending command")
	}
}

func TestShellWaitTimesOut(t *testing.T) {
	oldWaitTime := shellWaitTime
	shellWaitTime = 20 * time.Millisecond
	t.Cleanup(func() { shellWaitTime = oldWaitTime })

	process := &shellProcess{done: make(chan struct{}), output: []byte("pending output\n")}
	commands := shellCommands{active: map[string]*shellProcess{"pending": process}}

	started := time.Now()
	got, err := commands.wait([]string{"pending"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "wait timed out\ncommand still running: pending" {
		t.Fatalf("wait result = %q", got)
	}
	elapsed := time.Since(started)
	if elapsed < shellWaitTime || elapsed > time.Second {
		t.Fatalf("wait duration = %v, want between %v and one second", elapsed, shellWaitTime)
	}
	if commands.find("pending") != process {
		t.Fatal("wait retired a command on timeout")
	}
	process.mu.Lock()
	output := string(process.output)
	process.exited = true
	process.mu.Unlock()
	if output != "pending output\n" {
		t.Fatalf("wait consumed output on timeout: %q", output)
	}
	close(process.done)

	got, err = commands.wait([]string{"pending"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "command completed: pending\npending output\nexit status: 0" {
		t.Fatalf("completed wait result = %q", got)
	}
	if commands.find("pending") != nil {
		t.Fatal("completed command was not retired")
	}
}

func TestShellInputCapacityIncludesInFlightBytes(t *testing.T) {
	pipe := newBlockingWriteCloser()
	input := &shellInput{pipe: pipe}
	input.ready = sync.NewCond(&input.mu)
	done := make(chan struct{})
	go func() {
		input.loop()
		close(done)
	}()
	t.Cleanup(func() {
		input.stop()
		<-done
	})

	if err := input.enqueue(strings.Repeat("x", shellBufferSize), false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pipe.started:
	case <-time.After(time.Second):
		t.Fatal("stdin writer did not start")
	}
	if err := input.enqueue("x", false); err == nil {
		t.Fatal("write beyond the pending-byte limit was accepted")
	}
}

func TestHostShellWriteRequiresApproval(t *testing.T) {
	oldRunTime := shellRunTime
	shellRunTime = 20 * time.Millisecond
	t.Cleanup(func() { shellRunTime = oldRunTime })

	c := conversation{
		path:      filepath.Join(t.TempDir(), "chat"),
		hostInput: bufio.NewReader(strings.NewReader("n\n")),
	}
	openConversation(t, &c)
	if _, err := c.commands.run("host", true, shellLaunch{cmd: shellHelperCommand("echo")}, "run helper"); err != nil {
		t.Fatal(err)
	}
	call := toolCall{
		ID:   "write",
		Type: "function",
		Function: toolFunction{
			Name:      shellWriteTool.name,
			Arguments: `{"command_id":"host","input":"hello\n","close":false}`,
		},
	}
	closeOnly := call
	closeOnly.Function.Arguments = `{"command_id":"host","input":"","close":true}`
	if prepared := c.prepareTool(0, closeOnly, "tool_calls"); prepared.rejected != "" || prepared.prompt != "" {
		t.Fatalf("EOF-only host write requested approval: %#v", prepared)
	}
	var display bytes.Buffer
	updated, results, err := c.runTools([]toolCall{call}, "tool_calls", &display)
	if err != nil {
		t.Fatal(err)
	}
	if !updated[0].Rejected || !results[0].Transient {
		t.Fatalf("rejected write = %#v, %#v", updated, results)
	}
	if want := "¿ Write to unsandboxed command host:\n    hello\n  [y/N]"; !strings.Contains(display.String(), want) {
		t.Fatalf("approval prompt = %q, want %q", display.String(), want)
	}

	c.hostInput = bufio.NewReader(strings.NewReader("y\n"))
	call.ID = "approved-write"
	call.Function.Arguments = `{"command_id":"host","input":"hello\n","close":true}`
	_, results, err = c.runTools([]toolCall{call}, "tool_calls", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Content != "queued 6 bytes\nstdin close queued" {
		t.Fatalf("approved write result = %q", results[0].Content)
	}
	process := c.commands.find("host")
	select {
	case <-process.done:
	case <-time.After(2 * time.Second):
		t.Fatal("host command did not exit")
	}
	if got, err := c.commands.read("host"); err != nil || got != "hello\nexit status: 0" {
		t.Fatalf("host output = %q, %v", got, err)
	}
}

func TestShellEndTurnIsNonblocking(t *testing.T) {
	oldRunTime, oldGraceTime := shellRunTime, shellGraceTime
	shellRunTime = 20 * time.Millisecond
	shellGraceTime = 5 * time.Second
	t.Cleanup(func() {
		shellRunTime = oldRunTime
		shellGraceTime = oldGraceTime
	})

	var commands shellCommands
	got, err := commands.run("hang", true, shellLaunch{cmd: shellHelperCommand("hang")}, "run helper")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ready\ncommand still running: hang") {
		commands.shutdown()
		t.Fatalf("initial result = %q", got)
	}
	process := commands.find("hang")
	if process == nil {
		commands.shutdown()
		t.Fatal("running command was not retained")
	}

	returned := make(chan struct{})
	go func() {
		commands.endTurn()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		commands.shutdown()
		t.Fatal("endTurn blocked for the grace period")
	}
	commands.shutdown()
	select {
	case <-process.done:
	default:
		t.Fatal("shutdown did not synchronously reap the command")
	}
}

func TestDirectShellProcessGroupReceivesSignal(t *testing.T) {
	oldRunTime := shellRunTime
	shellRunTime = 20 * time.Millisecond
	t.Cleanup(func() { shellRunTime = oldRunTime })

	t.Chdir(t.TempDir())
	launch, err := hostShellCommand(`trap 'exit 0' TERM; printf ready; while :; do :; done`)
	if err != nil {
		t.Fatal(err)
	}
	var commands shellCommands
	t.Cleanup(commands.shutdown)
	got, err := commands.run("direct", false, launch, "run direct shell")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ready") {
		t.Fatalf("initial result = %q", got)
	}
	process := commands.find("direct")
	if process == nil {
		t.Fatal("direct process was not retained")
	}
	process.signal(syscall.SIGTERM)
	select {
	case <-process.done:
	case <-time.After(time.Second):
		t.Fatal("direct process did not receive SIGTERM")
	}
}

func TestShellTracksBackgroundProcessGroup(t *testing.T) {
	oldRunTime, oldGraceTime := shellRunTime, shellGraceTime
	shellRunTime = time.Second
	shellGraceTime = 20 * time.Millisecond
	t.Cleanup(func() {
		shellRunTime = oldRunTime
		shellGraceTime = oldGraceTime
	})

	var commands shellCommands
	t.Cleanup(commands.shutdown)
	got, err := commands.run("background", true, shellLaunch{cmd: shellHelperCommand("background")}, "run helper")
	if err != nil {
		t.Fatal(err)
	}
	pidText := strings.TrimSuffix(got, "\nexit status: 0")
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("result = %q: %v", got, err)
	}
	if commands.find("background") != nil {
		t.Fatal("completed command retained an interactive ID")
	}
	commands.endTurn()
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("background process %d survived turn cleanup: %v", pid, err)
	}
}

func TestShellSessionHelper(t *testing.T) {
	switch os.Getenv("A_SHELL_SESSION_HELPER") {
	case "echo":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		if _, err := os.Stdout.Write(data); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "hang":
		signal.Ignore(syscall.SIGTERM)
		if _, err := io.WriteString(os.Stdout, "ready\n"); err != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "background":
		devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			os.Exit(2)
		}
		cmd := shellHelperCommand("hang")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		if _, err := io.WriteString(os.Stdout, strconv.Itoa(cmd.Process.Pid)); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
}

func shellHelperCommand(mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestShellSessionHelper$")
	cmd.Env = append(os.Environ(), "A_SHELL_SESSION_HELPER="+mode)
	return cmd
}

type blockingWriteCloser struct {
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{started: make(chan struct{}), closed: make(chan struct{})}
}

func (w *blockingWriteCloser) Write([]byte) (int, error) {
	w.startOnce.Do(func() { close(w.started) })
	<-w.closed
	return 0, io.ErrClosedPipe
}

func (w *blockingWriteCloser) Close() error {
	w.closeOnce.Do(func() { close(w.closed) })
	return nil
}

var _ io.WriteCloser = (*blockingWriteCloser)(nil)
