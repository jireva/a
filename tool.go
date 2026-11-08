package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type toolDefinition struct {
	name        string
	description string
	parameters  json.RawMessage
}

var availableTools = []toolDefinition{shellTool, shellReadTool, shellWriteTool, shellWaitTool, hostShellTool}

type toolOutcome struct {
	index      int
	call       toolCall
	command    string
	commandID  string
	commandIDs []string
	input      string
	closeInput bool
	process    *shellProcess
	prompt     string
	approval   string
	content    string
	rejected   string
	err        error
}

func (c *conversation) runTools(calls []toolCall, finishReason string, display io.Writer) ([]toolCall, []message, error) {
	ids := make(map[string]bool, len(calls))
	for i, call := range calls {
		if call.ID == "" {
			return nil, nil, fmt.Errorf("tool call %d has no id", i+1)
		}
		if ids[call.ID] {
			return nil, nil, fmt.Errorf("tool call %q is duplicated", call.ID)
		}
		ids[call.ID] = true
	}

	prepared := make([]toolOutcome, len(calls))
	for i, call := range calls {
		prepared[i] = c.prepareTool(i, call, finishReason)
	}
	for i := range prepared {
		outcome := &prepared[i]
		if outcome.rejected != "" || outcome.prompt == "" {
			continue
		}
		prompt := outcome.prompt
		if _, err := io.WriteString(display, prompt+" "); err != nil {
			return nil, nil, fmt.Errorf("write approval: %w", err)
		}
		answer, err := c.readApproval(display, outcome.call.Function.Name)
		if err != nil {
			return nil, nil, err
		}
		outcome.approval = prompt + " " + strings.TrimRight(answer, "\r\n")
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			outcome.rejected = "host command was not approved"
			if outcome.call.Function.Name == shellWriteTool.name {
				outcome.rejected = "host command input was not approved"
			}
			outcome.content = "tool error: " + outcome.rejected
		}
	}

	done := make(chan toolOutcome, len(calls))
	needsSandbox := false
	for i := range prepared {
		if prepared[i].rejected != "" || prepared[i].call.Function.Name != shellTool.name || c.subagent {
			continue
		}
		needsSandbox = true
	}
	var sandbox *sandboxRunner
	var sandboxErr error
	if needsSandbox {
		sandbox, sandboxErr = newSandboxRunner()
		if sandboxErr == nil {
			sandbox.mounts = c.mounts
		}
		if sandboxErr == nil && c.ultra {
			sandboxErr = sandbox.enableSubagents(c.path+".s", c.server, c.model, c.effort)
		}
	}
	for _, outcome := range prepared {
		if outcome.rejected == "" && outcome.call.Function.Name == shellWaitTool.name {
			if err := c.recordWaitStart(outcome.commandIDs, display); err != nil {
				return nil, nil, err
			}
		}
	}

	for _, preparedOutcome := range prepared {
		go func(outcome toolOutcome) {
			if outcome.rejected != "" {
				done <- outcome
				return
			}
			switch outcome.call.Function.Name {
			case shellTool.name:
				if c.subagent {
					launch, err := hostShellCommand(outcome.command)
					if err != nil {
						outcome.err = err
					} else {
						outcome.content, outcome.err = c.commands.run(outcome.call.ID, false, launch, "run shell")
					}
				} else if sandboxErr != nil {
					outcome.err = sandboxErr
				} else {
					launch, err := sandbox.command(outcome.command)
					if err != nil {
						outcome.err = err
					} else {
						outcome.content, outcome.err = c.commands.run(outcome.call.ID, false, launch, "run sandboxed shell")
					}
				}
			case hostShellTool.name:
				launch, err := hostShellCommand(outcome.command)
				if err != nil {
					outcome.err = err
				} else {
					outcome.content, outcome.err = c.commands.run(outcome.call.ID, true, launch, "run host shell")
				}
			case shellReadTool.name:
				outcome.content, outcome.err = c.commands.read(outcome.commandID)
				if outcome.err != nil {
					outcome.content = "tool error: " + outcome.err.Error()
					outcome.err = nil
				}
			case shellWaitTool.name:
				outcome.content, outcome.err = c.commands.wait(outcome.commandIDs)
				if outcome.err != nil {
					outcome.content = "tool error: " + outcome.err.Error()
					outcome.err = nil
				}
			case shellWriteTool.name:
				if outcome.process == nil {
					outcome.content = fmt.Sprintf("tool error: unknown command id %q", outcome.commandID)
				} else {
					outcome.content, outcome.err = outcome.process.enqueue(outcome.input, outcome.closeInput)
					if outcome.err != nil {
						outcome.content = "tool error: " + outcome.err.Error()
						outcome.err = nil
					}
				}
			}
			done <- outcome
		}(preparedOutcome)
	}

	updated := append([]toolCall(nil), calls...)
	results := make([]message, 0, len(calls))
	var firstErr error
	for range calls {
		outcome := <-done
		if outcome.err != nil {
			if firstErr == nil {
				firstErr = outcome.err
			}
			continue
		}
		if outcome.rejected != "" {
			updated[outcome.index].Rejected = true
			var err error
			if outcome.approval == "" {
				err = c.recordRejected(outcome.call, outcome.rejected, display)
			} else {
				_, err = c.appendLive(outcome.approval+"\n", "")
			}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		} else if err := c.recordCommand(outcome, display); err != nil && firstErr == nil {
			firstErr = err
		}
		results = append(results, message{
			Role:       "tool",
			Content:    outcome.content,
			ToolCallID: outcome.call.ID,
			Transient:  outcome.rejected != "",
		})
	}
	if firstErr != nil {
		return nil, nil, firstErr
	}
	return updated, results, nil
}

func (c *conversation) prepareTool(index int, call toolCall, finishReason string) toolOutcome {
	outcome := toolOutcome{index: index, call: call}
	reject := func(reason string) toolOutcome {
		outcome.rejected = reason
		outcome.content = "tool error: " + reason
		return outcome
	}

	if finishReason == "length" || finishReason == "content_filter" {
		return reject("model response ended with finish reason " + finishReason)
	}
	if c.noTools {
		return reject("tools are disabled")
	}
	if call.Type != "function" {
		return reject(fmt.Sprintf("unsupported tool call type %q", call.Type))
	}
	switch call.Function.Name {
	case shellTool.name, hostShellTool.name:
		command, err := decodeShellArguments(call.Function.Arguments)
		if err != nil {
			return reject("invalid shell arguments: " + err.Error())
		}
		if call.Function.Name == hostShellTool.name {
			if c.hostInput == nil {
				return reject("host shell is unavailable without terminal input")
			}
			displayCommand := strings.TrimSuffix(command, "\n")
			outcome.prompt = "¿ Run unsandboxed:\n    " +
				strings.ReplaceAll(displayCommand, "\n", "\n    ") +
				"\n  [y/N]"
		}
		outcome.command = command

	case shellReadTool.name:
		id, err := decodeShellReadArguments(call.Function.Arguments)
		if err != nil {
			return reject("invalid shell_read arguments: " + err.Error())
		}
		outcome.commandID = id

	case shellWriteTool.name:
		arguments, err := decodeShellWriteArguments(call.Function.Arguments)
		if err != nil {
			return reject("invalid shell_write arguments: " + err.Error())
		}
		outcome.commandID = arguments.CommandID
		outcome.input = arguments.Input
		outcome.closeInput = arguments.Close
		outcome.process = c.commands.find(arguments.CommandID)
		if outcome.process != nil && outcome.process.host && arguments.Input != "" {
			if c.hostInput == nil {
				return reject("host shell input is unavailable without terminal input")
			}
			displayInput := strings.TrimSuffix(arguments.Input, "\n")
			outcome.prompt = "¿ Write to unsandboxed command " + arguments.CommandID + ":\n    " +
				strings.ReplaceAll(displayInput, "\n", "\n    ") +
				"\n  [y/N]"
		}

	case shellWaitTool.name:
		ids, err := decodeShellWaitArguments(call.Function.Arguments)
		if err != nil {
			return reject("invalid shell_wait arguments: " + err.Error())
		}
		outcome.commandIDs = ids

	default:
		return reject(fmt.Sprintf("unknown tool %q", call.Function.Name))
	}
	return outcome
}

func (c *conversation) readApproval(display io.Writer, name string) (string, error) {
	answer, err := c.hostInput.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read %s approval: %w", name, err)
	}
	if errors.Is(err, io.EOF) {
		if _, writeErr := io.WriteString(display, "\n"); writeErr != nil {
			return "", fmt.Errorf("write %s approval: %w", name, writeErr)
		}
	}
	return answer, nil
}

func (c *conversation) recordCommand(outcome toolOutcome, display io.Writer) error {
	header := ""
	liveHeader := outcome.approval
	switch outcome.call.Function.Name {
	case shellTool.name:
		header = blockHeader("$ ", outcome.command)
	case shellReadTool.name:
		header = "$>"
	case shellWriteTool.name:
		if outcome.approval == "" {
			header = shellWriteHeader(outcome.input, outcome.closeInput)
		}
	case shellWaitTool.name:
		header = "… Wait returned"
	}
	if header != "" {
		liveHeader = header
	}
	if !strings.HasSuffix(liveHeader, "\n") {
		liveHeader += "\n"
	}
	line, err := c.appendLive(liveHeader, outcome.content)
	if err != nil {
		return err
	}
	return writeLiveLink(display, header, c.path+".live", line)
}

func (c *conversation) recordWaitStart(ids []string, display io.Writer) error {
	noun := "commands"
	if len(ids) == 1 {
		noun = "command"
	}
	header := fmt.Sprintf("… Waiting on %d %s", len(ids), noun)
	line, err := c.appendLive(header+"\n", strings.Join(ids, "\n"))
	if err != nil {
		return err
	}
	return writeLiveLink(display, header, c.path+".live", line)
}

func shellWriteHeader(input string, closeInput bool) string {
	body := input
	if closeInput {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += "EOF"
	}
	return blockHeader("$< ", body)
}

func blockHeader(prefix, body string) string {
	body = strings.ReplaceAll(strings.TrimSuffix(body, "\n"), "\n", "\n  ")
	return prefix + body
}

func (c *conversation) recordRejected(call toolCall, reason string, display io.Writer) error {
	raw, err := json.Marshal(call)
	if err != nil {
		return fmt.Errorf("encode rejected tool call: %w", err)
	}
	line, err := c.appendLive("# rejected tool call\n", string(raw)+"\nerror: "+reason)
	if err != nil {
		return err
	}
	return writeLiveLink(display, "# rejected tool call", c.path+".live", line)
}

func (c *conversation) appendLive(header, body string) (int, error) {
	name := c.transcriptName + ".live"
	path := c.path + ".live"
	flags := os.O_WRONLY | os.O_APPEND
	if !c.live {
		if err := c.transcript.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("remove stale live transcript %q: %w", path, err)
		}
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := c.transcript.OpenFile(name, flags, 0666)
	if err != nil {
		return 0, fmt.Errorf("open live transcript %q: %w", path, err)
	}
	c.live = true
	if c.liveLine == 0 {
		c.liveLine = 1
	}

	line := c.liveLine + strings.Count(header, "\n")
	record := header + body
	if !strings.HasSuffix(record, "\n") {
		record += "\n"
	}
	record += "\n"
	_, writeErr := io.WriteString(f, record)
	closeErr := f.Close()
	if writeErr != nil {
		return 0, fmt.Errorf("write live transcript %q: %w", path, writeErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close live transcript %q: %w", path, closeErr)
	}
	c.liveLine += strings.Count(record, "\n")
	return line, nil
}

func (c *conversation) closeLive() error {
	c.commands.shutdown()
	return c.closeTranscript()
}

func (c *conversation) closeTranscript() error {
	if c.transcript == nil {
		return nil
	}
	c.live = false
	c.liveLine = 0
	removeErr := c.transcript.Remove(c.transcriptName + ".live")
	closeErr := c.transcript.Close()
	c.transcript = nil
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove live transcript: %w", removeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close transcript directory: %w", closeErr)
	}
	return nil
}

func writeLiveLink(w io.Writer, header, path string, line int) error {
	if header != "" {
		if _, err := io.WriteString(w, header); err != nil {
			return fmt.Errorf("write tool call: %w", err)
		}
		if !strings.HasSuffix(header, "\n") {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return fmt.Errorf("write tool call: %w", err)
			}
		}
	}
	if _, err := fmt.Fprintf(w, "  %s:%d\n\n", path, line); err != nil {
		return fmt.Errorf("write tool link: %w", err)
	}
	return nil
}
