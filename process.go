package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"
)

const shellBufferSize = 4 << 10

var (
	shellRunTime   = 20 * time.Second
	shellWaitTime  = 5 * time.Minute
	shellGraceTime = 20 * time.Second
)

type shellCommands struct {
	mu       sync.Mutex
	active   map[string]*shellProcess
	turn     map[*shellProcess]struct{}
	cleaning map[*shellProcess]struct{}
	closed   bool
}

type shellProcess struct {
	cmd   *exec.Cmd
	host  bool
	pid   int
	input *shellInput
	done  chan struct{}
	full  chan struct{}

	mu            sync.Mutex
	output        []byte
	dropped       int64
	written       int
	exited        bool
	status        int
	waitErr       error
	group         int
	groupGone     bool
	pendingSignal syscall.Signal
}

type shellInput struct {
	mu      sync.Mutex
	ready   *sync.Cond
	pipe    io.WriteCloser
	buffer  []byte
	pending int
	closing bool
	stopped bool
}

func (m *shellCommands) run(id string, host bool, launch shellLaunch, action string) (string, error) {
	defer launch.closeGroupPipe()
	m.mu.Lock()
	busy := m.active[id] != nil
	closed := m.closed
	m.mu.Unlock()
	if busy {
		return fmt.Sprintf("tool error: command id %q is already running", id), nil
	}
	if closed {
		return "", errors.New("shell command manager is closed")
	}

	cmd := launch.cmd
	inputPipe, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("%s: open stdin: %w", action, err)
	}
	input := &shellInput{pipe: inputPipe}
	input.ready = sync.NewCond(&input.mu)
	process := &shellProcess{
		cmd:   cmd,
		host:  host,
		input: input,
		done:  make(chan struct{}),
		full:  make(chan struct{}),
	}
	cmd.Stdout = process
	cmd.Stderr = process
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: host, Setpgid: !host}
	if err := cmd.Start(); err != nil {
		input.stop()
		return "", fmt.Errorf("%s: %w", action, err)
	}
	launch.closeGroupPipe()
	process.pid = cmd.Process.Pid
	if host || launch.group == nil {
		process.group = process.pid
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		go input.loop()
		go process.wait(m, action, launch.group)
		process.signal(syscall.SIGKILL)
		<-process.done
		return "", errors.New("shell command manager is closed")
	}
	if m.turn == nil {
		m.turn = make(map[*shellProcess]struct{})
	}
	m.turn[process] = struct{}{}
	m.mu.Unlock()

	go input.loop()
	go process.wait(m, action, launch.group)

	select {
	case <-process.done:
	case <-process.full:
	case <-time.After(shellRunTime):
	}

	result, exited, err := process.takeResult(id)
	if err != nil {
		return "", err
	}
	if exited {
		return result, nil
	}

	m.mu.Lock()
	if m.active == nil {
		m.active = make(map[string]*shellProcess)
	}
	if m.active[id] != nil {
		m.mu.Unlock()
		process.stop()
		process.signal(syscall.SIGKILL)
		return fmt.Sprintf("tool error: command id %q is already running", id), nil
	}
	m.active[id] = process
	m.mu.Unlock()
	return result, nil
}

func (m *shellCommands) find(id string) *shellProcess {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[id]
}

func (m *shellCommands) read(id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	process := m.active[id]
	if process == nil {
		return "", fmt.Errorf("unknown command id %q", id)
	}
	result, exited, err := process.takeResult(id)
	if exited {
		delete(m.active, id)
	}
	return result, err
}

func (m *shellCommands) wait(ids []string) (string, error) {
	m.mu.Lock()
	processes := make([]*shellProcess, len(ids))
	for i, id := range ids {
		processes[i] = m.active[id]
		if processes[i] == nil {
			m.mu.Unlock()
			return "", fmt.Errorf("unknown command id %q", id)
		}
	}
	m.mu.Unlock()

	timer := time.NewTimer(shellWaitTime)
	defer timer.Stop()
	cases := make([]reflect.SelectCase, len(processes)+1)
	for i, process := range processes {
		cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(process.done)}
	}
	cases[len(processes)] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(timer.C)}
	selected, _, _ := reflect.Select(cases)
	timedOut := selected == len(processes)

	m.mu.Lock()
	defer m.mu.Unlock()
	var result strings.Builder
	var running []string
	for i, process := range processes {
		process.mu.Lock()
		exited := process.exited
		process.mu.Unlock()
		if !exited {
			if timedOut {
				running = append(running, ids[i])
			}
			continue
		}
		output, _, err := process.takeResult(ids[i])
		delete(m.active, ids[i])
		if err != nil {
			return "", err
		}
		if result.Len() != 0 {
			result.WriteString("\n\n")
		}
		fmt.Fprintf(&result, "command completed: %s\n", ids[i])
		result.WriteString(output)
	}
	if len(running) != 0 {
		if result.Len() != 0 {
			result.WriteString("\n\n")
		}
		result.WriteString("wait timed out")
		for _, id := range running {
			fmt.Fprintf(&result, "\ncommand still running: %s", id)
		}
	}
	return result.String(), nil
}

func (m *shellCommands) endTurn() {
	m.mu.Lock()
	if len(m.turn) == 0 {
		m.active = nil
		m.mu.Unlock()
		return
	}
	processes := make([]*shellProcess, 0, len(m.turn))
	if m.cleaning == nil {
		m.cleaning = make(map[*shellProcess]struct{})
	}
	for process := range m.turn {
		processes = append(processes, process)
		m.cleaning[process] = struct{}{}
	}
	m.turn = nil
	m.active = nil
	m.mu.Unlock()

	for _, process := range processes {
		process.stop()
		process.signal(syscall.SIGTERM)
	}
	time.AfterFunc(shellGraceTime, func() {
		m.mu.Lock()
		pending := make([]*shellProcess, 0, len(processes))
		if !m.closed {
			for _, process := range processes {
				if _, ok := m.cleaning[process]; ok {
					pending = append(pending, process)
				}
			}
		}
		m.mu.Unlock()
		for _, process := range pending {
			process.signal(syscall.SIGKILL)
		}
		for _, process := range pending {
			<-process.done
			process.markGroupGone()
		}
		m.mu.Lock()
		for _, process := range pending {
			delete(m.cleaning, process)
		}
		m.mu.Unlock()
	})
}

func (m *shellCommands) shutdown() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	set := make(map[*shellProcess]struct{}, len(m.turn)+len(m.cleaning))
	for process := range m.turn {
		set[process] = struct{}{}
	}
	for process := range m.cleaning {
		set[process] = struct{}{}
	}
	m.turn = nil
	m.cleaning = nil
	m.active = nil
	m.mu.Unlock()

	for process := range set {
		process.stop()
		process.signal(syscall.SIGKILL)
	}
	for process := range set {
		<-process.done
		process.markGroupGone()
	}
}

func (m *shellCommands) forgetFinished(process *shellProcess) {
	if process.groupAlive() {
		return
	}
	process.markGroupGone()
	m.mu.Lock()
	delete(m.turn, process)
	delete(m.cleaning, process)
	m.mu.Unlock()
}

func (p *shellProcess) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(data)
	if p.written < shellBufferSize {
		p.written += n
		if p.written >= shellBufferSize {
			close(p.full)
		}
	}
	if excess := len(p.output) + n - shellBufferSize; excess > 0 {
		p.dropped += int64(excess)
		if excess >= len(p.output) {
			data = data[excess-len(p.output):]
			p.output = p.output[:0]
		} else {
			copy(p.output, p.output[excess:])
			p.output = p.output[:len(p.output)-excess]
		}
	}
	p.output = append(p.output, data...)
	return n, nil
}

func (p *shellProcess) wait(manager *shellCommands, action string, group <-chan int) {
	if group != nil {
		p.setGroup(<-group)
	}
	err := p.cmd.Wait()
	status := 0
	var waitErr error
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			status = exitError.ExitCode()
		} else {
			waitErr = fmt.Errorf("%s: %w", action, err)
		}
	}
	p.mu.Lock()
	p.exited = true
	p.status = status
	p.waitErr = waitErr
	p.mu.Unlock()
	p.input.stop()
	manager.forgetFinished(p)
	close(p.done)
}

func (p *shellProcess) takeResult(id string) (string, bool, error) {
	p.mu.Lock()
	output := append([]byte(nil), p.output...)
	dropped := p.dropped
	p.output = p.output[:0]
	p.dropped = 0
	exited := p.exited
	status := p.status
	err := p.waitErr
	p.mu.Unlock()
	if err != nil {
		return "", exited, err
	}
	return formatShellResult(output, dropped, exited, status, id), exited, nil
}

func (p *shellProcess) stop() {
	p.input.stop()
}

func (p *shellProcess) signal(signal syscall.Signal) {
	p.mu.Lock()
	if p.groupGone {
		p.mu.Unlock()
		return
	}
	if p.pendingSignal != syscall.SIGKILL {
		p.pendingSignal = signal
	}
	group := p.group
	p.mu.Unlock()

	if group != 0 {
		_ = syscall.Kill(-group, signal)
	}
	if !p.host && signal == syscall.SIGKILL {
		_ = syscall.Kill(-p.pid, signal)
	}
}

func (p *shellProcess) groupAlive() bool {
	p.mu.Lock()
	group := p.group
	groupGone := p.groupGone
	p.mu.Unlock()
	if groupGone {
		return false
	}
	if group == 0 {
		group = p.pid
	}
	if group == 0 {
		return false
	}
	err := syscall.Kill(-group, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (p *shellProcess) setGroup(group int) {
	if group <= 0 {
		return
	}
	p.mu.Lock()
	if p.groupGone {
		p.mu.Unlock()
		return
	}
	p.group = group
	signal := p.pendingSignal
	p.mu.Unlock()
	if signal != 0 {
		_ = syscall.Kill(-group, signal)
	}
}

func (p *shellProcess) markGroupGone() {
	p.mu.Lock()
	p.groupGone = true
	p.mu.Unlock()
}

func (p *shellProcess) enqueue(input string, closeInput bool) (string, error) {
	if err := p.input.enqueue(input, closeInput); err != nil {
		return "", err
	}
	var result strings.Builder
	if input != "" {
		fmt.Fprintf(&result, "queued %d bytes", len(input))
	}
	if closeInput {
		if result.Len() != 0 {
			result.WriteByte('\n')
		}
		result.WriteString("stdin close queued")
	}
	return result.String(), nil
}

func (in *shellInput) enqueue(input string, closeInput bool) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.stopped || in.closing {
		return errors.New("command stdin is closed")
	}
	available := shellBufferSize - in.pending
	if len(input) > available {
		return fmt.Errorf("command stdin accepts at most %d more bytes", available)
	}
	in.buffer = append(in.buffer, input...)
	in.pending += len(input)
	in.closing = closeInput
	in.ready.Signal()
	return nil
}

func (in *shellInput) loop() {
	for {
		in.mu.Lock()
		for len(in.buffer) == 0 && !in.closing && !in.stopped {
			in.ready.Wait()
		}
		if in.stopped {
			in.mu.Unlock()
			return
		}
		data := in.buffer
		in.buffer = nil
		closeInput := in.closing
		in.mu.Unlock()

		_, err := io.Copy(in.pipe, bytes.NewReader(data))
		in.mu.Lock()
		if !in.stopped {
			in.pending -= len(data)
		}
		in.mu.Unlock()
		if err != nil || closeInput {
			in.stop()
			return
		}
	}
}

func (in *shellInput) stop() {
	in.mu.Lock()
	if in.stopped {
		in.mu.Unlock()
		return
	}
	in.stopped = true
	in.buffer = nil
	in.pending = 0
	in.ready.Broadcast()
	pipe := in.pipe
	in.mu.Unlock()
	_ = pipe.Close()
}

func formatShellResult(output []byte, dropped int64, exited bool, status int, id string) string {
	output = bytes.ToValidUTF8(output, []byte("\uFFFD"))
	var result strings.Builder
	if dropped != 0 {
		fmt.Fprintf(&result, "output discarded: %d bytes\n", dropped)
	}
	result.Write(output)
	if len(output) != 0 && output[len(output)-1] != '\n' {
		result.WriteByte('\n')
	}
	if exited {
		fmt.Fprintf(&result, "exit status: %d", status)
	} else {
		fmt.Fprintf(&result, "command still running: %s", id)
	}
	return result.String()
}
