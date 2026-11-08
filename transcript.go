package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func parseTranscript(data []byte) ([]message, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if bad := invalidUTF8Offset(data); bad >= 0 {
		line := bytes.Count(data[:bad], []byte{'\n'}) + 1
		return nil, fmt.Errorf("line %d: invalid UTF-8", line)
	}
	if data[len(data)-1] != '\n' {
		line := bytes.Count(data, []byte{'\n'}) + 1
		return nil, fmt.Errorf("line %d: line is not LF-terminated", line)
	}

	lines := bytes.Split(data[:len(data)-1], []byte{'\n'})
	type record struct {
		tag     string
		content string
		line    int
	}
	var records []record
	for i := 0; i < len(lines); {
		line := i + 1
		if bytes.Equal(lines[i], []byte("compaction:")) {
			if i != len(lines)-1 {
				return nil, fmt.Errorf("line %d: compaction must be the final record", line)
			}
			records = append(records, record{tag: "compaction", line: line})
			i++
			continue
		}
		tag, content, next, err := parseTranscriptRecord(lines, i)
		if err != nil {
			return nil, err
		}
		records = append(records, record{tag: tag, content: content, line: line})
		i = next
	}

	messages := make([]message, 0, len(records))
	inTurn := false
	lastAssistant := false
	summary := false
	toolNumber := 0
	for i := 0; i < len(records); i++ {
		record := records[i]
		switch record.tag {
		case "summary":
			if i != 0 {
				return nil, fmt.Errorf("line %d: summary must be the first record", record.line)
			}
			messages = append(messages, message{Role: "user", Content: record.content, Summary: true})
			summary = true

		case "user":
			if inTurn && !lastAssistant {
				return nil, fmt.Errorf("line %d: previous turn is incomplete", record.line)
			}
			messages = append(messages, message{Role: "user", Content: record.content})
			inTurn = true
			lastAssistant = false

		case "assistant":
			if !inTurn && !summary {
				return nil, fmt.Errorf("line %d: assistant appears outside a turn", record.line)
			}
			inTurn = true
			messages = append(messages, message{Role: "assistant", Content: record.content})
			lastAssistant = true

		case "shell tool", "host shell tool", "shell read tool", "shell write tool", "shell wait tool":
			if !inTurn && !summary {
				return nil, fmt.Errorf("line %d: tool appears outside a turn", record.line)
			}
			inTurn = true
			if i+1 == len(records) || records[i+1].tag != "output" {
				return nil, fmt.Errorf("line %d: %s is not followed by output", record.line, record.tag)
			}
			name := shellTool.name
			arguments := record.content
			switch record.tag {
			case "shell tool", "host shell tool":
				encoded, err := json.Marshal(struct {
					Command string `json:"command"`
				}{Command: record.content})
				if err != nil {
					return nil, fmt.Errorf("line %d: encode tool arguments: %w", record.line, err)
				}
				arguments = string(encoded)
				if record.tag == "host shell tool" {
					name = hostShellTool.name
				}
			case "shell read tool":
				name = shellReadTool.name
				if _, err := decodeShellReadArguments(arguments); err != nil {
					return nil, fmt.Errorf("line %d: invalid %s arguments: %w", record.line, name, err)
				}
			case "shell write tool":
				name = shellWriteTool.name
				if _, err := decodeShellWriteArguments(arguments); err != nil {
					return nil, fmt.Errorf("line %d: invalid %s arguments: %w", record.line, name, err)
				}
			case "shell wait tool":
				name = shellWaitTool.name
				if _, err := decodeShellWaitArguments(arguments); err != nil {
					return nil, fmt.Errorf("line %d: invalid %s arguments: %w", record.line, name, err)
				}
			}
			toolNumber++
			id := fmt.Sprintf("transcript-tool-%d", toolNumber)
			messages = append(messages,
				message{
					Role: "assistant",
					ToolCalls: []toolCall{{
						ID:   id,
						Type: "function",
						Function: toolFunction{
							Name:      name,
							Arguments: string(arguments),
						},
					}},
				},
				message{Role: "tool", Content: records[i+1].content, ToolCallID: id},
			)
			lastAssistant = false
			i++

		case "output":
			return nil, fmt.Errorf("line %d: output has no preceding tool", record.line)

		case "compaction":
			messages = append(messages, message{Compaction: true})
			return messages, nil

		default:
			return nil, fmt.Errorf("line %d: unknown record %q", record.line, record.tag)
		}
	}
	if inTurn && !lastAssistant {
		return nil, errors.New("transcript has an incomplete turn")
	}
	return messages, nil
}

func parseTranscriptRecord(lines [][]byte, i int) (string, string, int, error) {
	if i == len(lines) || !bytes.HasSuffix(lines[i], []byte{':'}) || len(lines[i]) == 1 {
		return "", "", i, fmt.Errorf("line %d: expected record header", i+1)
	}
	tag := string(lines[i][:len(lines[i])-1])
	i++

	var content bytes.Buffer
	bodyLines := 0
	for {
		if i == len(lines) {
			return "", "", i, fmt.Errorf("line %d: missing message terminator", i+1)
		}
		line := lines[i]
		lineNumber := i + 1
		i++
		if bytes.Equal(line, []byte{'.'}) {
			if bodyLines == 0 {
				return "", "", i, fmt.Errorf("line %d: missing framing line before terminator", lineNumber)
			}
			body := content.Bytes()
			return tag, string(body[:len(body)-1]), i, nil
		}
		if allDots(line) && len(line) >= 2 {
			line = line[1:]
		}
		content.Write(line)
		content.WriteByte('\n')
		bodyLines++
	}
}

func formatTranscript(messages []message) ([]byte, error) {
	var out bytes.Buffer
	pending := make(map[string]toolCall)
	inTurn := false
	lastAssistant := false
	summary := false
	for i, m := range messages {
		if !utf8.ValidString(m.Content) {
			return nil, fmt.Errorf("message %d: invalid UTF-8", i+1)
		}
		if m.Compaction {
			if i != len(messages)-1 {
				return nil, fmt.Errorf("message %d: compaction must be the final message", i+1)
			}
			if m.Summary || m.Role != "" || m.Content != "" || m.Reasoning != "" || len(m.ToolCalls) != 0 || m.ToolCallID != "" || m.Transient {
				return nil, fmt.Errorf("message %d: invalid compaction marker", i+1)
			}
			if len(pending) != 0 {
				return nil, fmt.Errorf("message %d: incomplete tool call", i+1)
			}
			out.WriteString("compaction:\n")
			return out.Bytes(), nil
		}
		if m.Summary {
			if i != 0 {
				return nil, fmt.Errorf("message %d: summary must be the first message", i+1)
			}
			if m.Role != "user" || len(m.ToolCalls) != 0 || m.ToolCallID != "" || m.Transient {
				return nil, fmt.Errorf("message %d: invalid summary message", i+1)
			}
			writeTranscriptRecord(&out, "summary", m.Content)
			summary = true
			continue
		}

		switch m.Role {
		case "user":
			if len(m.ToolCalls) != 0 || m.ToolCallID != "" {
				return nil, fmt.Errorf("message %d: invalid user message", i+1)
			}
			if len(pending) != 0 || inTurn && !lastAssistant {
				return nil, fmt.Errorf("message %d: previous turn is incomplete", i+1)
			}
			writeTranscriptRecord(&out, "user", m.Content)
			inTurn = true
			lastAssistant = false

		case "assistant":
			if (!inTurn && !summary) || len(pending) != 0 || m.ToolCallID != "" {
				return nil, fmt.Errorf("message %d: invalid assistant position", i+1)
			}
			inTurn = true
			if m.Content != "" || len(m.ToolCalls) == 0 {
				writeTranscriptRecord(&out, "assistant", m.Content)
				lastAssistant = true
			} else {
				lastAssistant = false
			}
			for _, call := range m.ToolCalls {
				if call.Rejected {
					continue
				}
				if call.ID == "" {
					return nil, fmt.Errorf("message %d: tool call has no id", i+1)
				}
				if _, exists := pending[call.ID]; exists {
					return nil, fmt.Errorf("message %d: duplicate tool call id %q", i+1, call.ID)
				}
				pending[call.ID] = call
			}
			if len(m.ToolCalls) != 0 {
				lastAssistant = false
			}

		case "tool":
			if m.Transient {
				continue
			}
			if !inTurn || len(m.ToolCalls) != 0 {
				return nil, fmt.Errorf("message %d: invalid tool result", i+1)
			}
			call, ok := pending[m.ToolCallID]
			if !ok {
				return nil, fmt.Errorf("message %d: tool result has no matching call", i+1)
			}
			if call.Type != "function" {
				return nil, fmt.Errorf("message %d: unsupported tool %q", i+1, call.Function.Name)
			}
			tag := ""
			content := call.Function.Arguments
			switch call.Function.Name {
			case shellTool.name, hostShellTool.name:
				var err error
				content, err = decodeShellArguments(call.Function.Arguments)
				if err != nil {
					return nil, fmt.Errorf("message %d: invalid %s arguments: %w", i+1, call.Function.Name, err)
				}
				tag = "shell tool"
				if call.Function.Name == hostShellTool.name {
					tag = "host shell tool"
				}
			case shellReadTool.name:
				if _, err := decodeShellReadArguments(call.Function.Arguments); err != nil {
					return nil, fmt.Errorf("message %d: invalid %s arguments: %w", i+1, call.Function.Name, err)
				}
				tag = "shell read tool"
			case shellWriteTool.name:
				if _, err := decodeShellWriteArguments(call.Function.Arguments); err != nil {
					return nil, fmt.Errorf("message %d: invalid %s arguments: %w", i+1, call.Function.Name, err)
				}
				tag = "shell write tool"
			case shellWaitTool.name:
				if _, err := decodeShellWaitArguments(call.Function.Arguments); err != nil {
					return nil, fmt.Errorf("message %d: invalid %s arguments: %w", i+1, call.Function.Name, err)
				}
				tag = "shell wait tool"
			default:
				return nil, fmt.Errorf("message %d: unsupported tool %q", i+1, call.Function.Name)
			}
			writeTranscriptRecord(&out, tag, content)
			writeTranscriptRecord(&out, "output", m.Content)
			delete(pending, m.ToolCallID)
			lastAssistant = false

		default:
			return nil, fmt.Errorf("message %d: unknown role %q", i+1, m.Role)
		}
	}
	if len(pending) != 0 || inTurn && !lastAssistant {
		return nil, errors.New("transcript has an incomplete turn")
	}
	return out.Bytes(), nil
}

func writeTranscriptRecord(out *bytes.Buffer, tag, content string) {
	out.WriteString(tag)
	out.WriteString(":\n")
	for _, line := range strings.Split(content, "\n") {
		if allDots([]byte(line)) && line != "" {
			out.WriteByte('.')
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	out.WriteString(".\n")
}

func allDots(line []byte) bool {
	for _, b := range line {
		if b != '.' {
			return false
		}
	}
	return true
}

func invalidUTF8Offset(data []byte) int {
	for i := 0; i < len(data); {
		_, size := utf8.DecodeRune(data[i:])
		if size == 1 && data[i] >= utf8.RuneSelf {
			return i
		}
		i += size
	}
	return -1
}

func resolveTranscriptPath(name string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", fmt.Errorf("resolve transcript path: %w", err)
	}
	parent := filepath.Dir(abs)

	if _, err := os.Lstat(abs); err == nil {
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", fmt.Errorf("resolve transcript %q: %w", abs, err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return "", fmt.Errorf("inspect transcript %q: %w", resolved, err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("transcript %q is not a regular file", resolved)
		}
		return resolved, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect transcript %q: %w", abs, err)
	}

	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve transcript directory %q: %w", parent, err)
	}
	return filepath.Join(resolvedParent, filepath.Base(abs)), nil
}

func (c *conversation) openTranscript() error {
	if c.transcript != nil {
		return nil
	}
	root, err := os.OpenRoot(filepath.Dir(c.path))
	if err != nil {
		return fmt.Errorf("open transcript directory: %w", err)
	}
	c.transcript = root
	c.transcriptName = filepath.Base(c.path)
	return nil
}

func replaceTranscript(root *os.Root, name, path string, messages []message) (int, error) {
	data, err := formatTranscript(messages)
	if err != nil {
		return 0, fmt.Errorf("format transcript: %w", err)
	}
	assistantLine := 0
	if len(messages) != 0 && messages[len(messages)-1].Role == "assistant" {
		content := messages[len(messages)-1].Content
		assistantLine = bytes.Count(data, []byte("\n")) - strings.Count(content, "\n") - 1
	}

	mode := os.FileMode(0666)
	preserveMode := false
	if info, err := root.Stat(name); err == nil {
		mode = info.Mode().Perm()
		preserveMode = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("inspect transcript %q: %w", path, err)
	}

	temp := name + "---writing"
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, fmt.Errorf("create temporary transcript %q: %w", path+"---writing", err)
	}
	fail := func(action string, actionErr error) error {
		f.Close()
		return fmt.Errorf("%s temporary transcript %q: %w", action, path+"---writing", actionErr)
	}
	if preserveMode {
		if err := f.Chmod(mode); err != nil {
			return 0, fail("set permissions on", err)
		}
	}
	if _, err := f.Write(data); err != nil {
		return 0, fail("write", err)
	}
	if err := f.Sync(); err != nil {
		return 0, fail("sync", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("close temporary transcript %q: %w", path+"---writing", err)
	}
	if err := root.Rename(temp, name); err != nil {
		return 0, fmt.Errorf("replace transcript %q: %w", path, err)
	}
	dir, err := root.Open(".")
	if err != nil {
		return 0, fmt.Errorf("open transcript directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return 0, fmt.Errorf("sync transcript directory: %w", err)
	}
	return assistantLine, nil
}
