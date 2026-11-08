package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const maxRequestBytes = 400000

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
	Rejected bool         `json:"-"`
}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Reasoning  string     `json:"reasoning_content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Transient  bool       `json:"-"`
	Summary    bool       `json:"-"`
	Compaction bool       `json:"-"`
}

type conversation struct {
	name           string
	path           string
	loaded         bool
	messages       []message
	server         string
	model          string
	effort         string
	noTools        bool
	ultra          bool
	subagent       bool
	mounts         []sandboxMount
	hostInput      *bufio.Reader
	transcript     *os.Root
	transcriptName string
	compacted      bool
	live           bool
	liveLine       int
	assistantLine  int
	status         io.Writer
	nextTranscript func() (string, error)
	commands       shellCommands
}

func (c *conversation) turn(prompt string, out, reasoningOut io.Writer) (string, error) {
	defer c.commands.endTurn()
	if !utf8.ValidString(prompt) {
		return "", errors.New("prompt is not valid UTF-8")
	}
	if err := c.load(); err != nil {
		return "", err
	}
	instructions := agentInstructions(c.ultra, c.subagent)
	if instructions != "" && !hasAgentInstructions(c.messages, instructions) {
		prompt = agentPrompt(prompt, c.ultra, c.subagent)
	}

	history := append([]message(nil), c.messages...)
	boundary := len(history)
	history = append(history, message{Role: "user", Content: prompt})
	definitions := availableTools
	if c.hostInput == nil || c.subagent {
		definitions = definitions[:len(definitions)-1]
	}
	if c.noTools {
		definitions = nil
	}
	var emitted strings.Builder
	forceCompaction := c.compacted
	for {
		body, err := encodeRequest(c.model, c.effort, history, definitions)
		if err != nil {
			return "", err
		}
		if forceCompaction || len(body) > maxRequestBytes {
			history, err = c.compact(history, boundary, definitions)
			if err != nil {
				return "", fmt.Errorf("compact transcript: %w", err)
			}
			forceCompaction = false
		}

		completion, err := streamCompletion(c.server, c.model, c.effort, history, definitions, out, reasoningOut, c.status)
		if err != nil {
			return "", err
		}
		response := completion.Message
		emitted.WriteString(response.Content)
		if len(response.ToolCalls) == 0 {
			c.commands.endTurn()
			history = append(history, response)
			for i := range history {
				history[i].Reasoning = ""
			}
			line, err := replaceTranscript(c.transcript, c.transcriptName, c.path, history)
			if err != nil {
				return "", err
			}
			c.messages = history
			c.assistantLine = line
			return emitted.String(), nil
		}

		if response.Content != "" {
			if err := responseGap(reasoningOut, response.Content); err != nil {
				return "", fmt.Errorf("write assistant gap: %w", err)
			}
		}
		calls, results, err := c.runTools(response.ToolCalls, completion.FinishReason, reasoningOut)
		if err != nil {
			return "", err
		}
		response.ToolCalls = calls
		boundary = len(history)
		history = append(history, response)
		history = append(history, results...)
	}
}

func responseGap(w io.Writer, response string) error {
	newlines := 0
	for i := len(response); i > 0 && response[i-1] == '\n' && newlines < 2; {
		newlines++
		i--
		if i > 0 && response[i-1] == '\r' {
			i--
		}
	}
	_, err := io.WriteString(w, strings.Repeat("\n", 2-newlines))
	return err
}

func (c *conversation) compact(history []message, boundary int, definitions []toolDefinition) ([]message, error) {
	if boundary == 0 {
		return nil, errors.New("transcript has no history to compact")
	}
	status := c.status
	if status == nil {
		status = io.Discard
	}
	if _, err := fmt.Fprintf(status, "… Compacting transcript\n  %s\n\n", c.path); err != nil {
		return nil, fmt.Errorf("write compaction status: %w", err)
	}

	request := append([]message(nil), history[:boundary]...)
	request = append(request, message{Role: "user", Content: compactionPrompt})
	result, err := streamCompletion(c.server, c.model, c.effort, request, nil, io.Discard, io.Discard, status)
	if err != nil {
		return nil, err
	}
	if result.FinishReason != "stop" {
		return nil, fmt.Errorf("checkpoint ended with finish reason %q", result.FinishReason)
	}
	if len(result.Message.ToolCalls) != 0 {
		return nil, errors.New("checkpoint contains tool calls")
	}
	if strings.TrimSpace(result.Message.Content) == "" {
		return nil, errors.New("checkpoint is empty")
	}

	summary := message{
		Role:    "user",
		Content: summaryBlock(c.path, result.Message.Content, agentInstructions(c.ultra, c.subagent)),
		Summary: true,
	}
	next := make([]message, 1, 1+len(history)-boundary)
	next[0] = summary
	next = append(next, history[boundary:]...)
	body, err := encodeRequest(c.model, c.effort, next, definitions)
	if err != nil {
		return nil, err
	}
	if len(body) > maxRequestBytes {
		return nil, errors.New("checkpoint does not reduce the request below the context limit")
	}

	if c.nextTranscript == nil {
		return nil, errors.New("no transcript allocator")
	}
	path := c.path
	for path == c.path {
		name, err := c.nextTranscript()
		if err != nil {
			return nil, err
		}
		path, err = resolveTranscriptPath(name)
		if err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open transcript directory: %w", err)
	}
	newName := filepath.Base(path)
	if _, err := replaceTranscript(root, newName, path, []message{summary}); err != nil {
		root.Close()
		return nil, err
	}

	old := history[:boundary]
	if history[boundary].Role != "user" {
		old = history
	}
	if !c.compacted {
		closed := append([]message(nil), old...)
		closed = append(closed, message{Compaction: true})
		if _, err := replaceTranscript(c.transcript, c.transcriptName, c.path, closed); err != nil {
			root.Remove(newName)
			root.Close()
			return nil, err
		}
	}

	closeErr := c.closeTranscript()
	c.name = path
	c.path = path
	c.messages = []message{summary}
	c.transcript = root
	c.transcriptName = newName
	c.compacted = false
	if _, err := fmt.Fprintf(status, "… Continuing in transcript\n  %s\n\n", path); err != nil {
		return nil, fmt.Errorf("write compaction status: %w", err)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return next, nil
}

func summaryBlock(path, checkpoint, instructions string) string {
	var summary strings.Builder
	summary.WriteString("You are continuing a session whose earlier conversation was compacted into the checkpoint below.\n\n")
	summary.WriteString("The full transcript before compaction is:\n")
	summary.WriteString(path)
	summary.WriteString("\n\nRead that transcript if more detail is needed.\n")
	if instructions != "" {
		summary.WriteString("\nStanding harness instructions:\n<system>\n")
		summary.WriteString(instructions)
		summary.WriteString("\n</system>\n")
	}
	summary.WriteString("\nCheckpoint:\n")
	summary.WriteString(checkpoint)
	return summary.String()
}

func (c *conversation) load() error {
	if c.loaded {
		return nil
	}
	path, err := resolveTranscriptPath(c.name)
	if err != nil {
		return err
	}
	c.path = path
	if err := c.openTranscript(); err != nil {
		return err
	}

	data, err := c.transcript.ReadFile(c.transcriptName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.loaded = true
			return nil
		}
		return fmt.Errorf("read transcript %q: %w", path, err)
	}
	c.messages, err = parseTranscript(data)
	if err != nil {
		return fmt.Errorf("parse transcript %q: %w", path, err)
	}
	if len(c.messages) != 0 && c.messages[len(c.messages)-1].Compaction {
		c.compacted = true
		c.messages = c.messages[:len(c.messages)-1]
	}
	c.loaded = true
	return nil
}
