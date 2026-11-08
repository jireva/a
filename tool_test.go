package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type noticeWriter struct {
	bytes.Buffer
	notices chan struct{}
}

func (w *noticeWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if bytes.Contains(p, []byte(".live:")) {
		w.notices <- struct{}{}
	}
	return n, err
}

func openConversation(t *testing.T, c *conversation) {
	t.Helper()
	if err := c.openTranscript(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.closeLive(); err != nil {
			t.Error(err)
		}
	})
}

func TestDecodeShellArguments(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"command", `{"command":"printf ok"}`, "printf ok", true},
		{"empty command", `{"command":""}`, "", true},
		{"missing command", `{}`, "", false},
		{"null command", `{"command":null}`, "", false},
		{"wrong type", `{"command":1}`, "", false},
		{"unknown field", `{"command":"x","cwd":"/"}`, "", false},
		{"trailing value", `{"command":"x"} {}`, "", false},
		{"malformed", `{"command":`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeShellArguments(test.raw)
			if (err == nil) != test.ok {
				t.Fatalf("decodeShellArguments(%q) error = %v, want success %v", test.raw, err, test.ok)
			}
			if got != test.want {
				t.Fatalf("decodeShellArguments(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

func TestDecodeShellInteractionArguments(t *testing.T) {
	if id, err := decodeShellReadArguments(`{"command_id":"c1"}`); err != nil || id != "c1" {
		t.Fatalf("decodeShellReadArguments() = %q, %v", id, err)
	}
	for _, raw := range []string{`{}`, `{"command_id":""}`, `{"command_id":"c1","extra":1}`} {
		if _, err := decodeShellReadArguments(raw); err == nil {
			t.Fatalf("decodeShellReadArguments(%q) succeeded", raw)
		}
	}

	want := shellWriteArguments{CommandID: "c1", Input: "x\n", Close: true}
	if got, err := decodeShellWriteArguments(`{"command_id":"c1","input":"x\n","close":true}`); err != nil || got != want {
		t.Fatalf("decodeShellWriteArguments() = %#v, %v", got, err)
	}
	for _, raw := range []string{`{"command_id":"c1"}`, `{"command_id":"","close":true}`, `{"command_id":"c1","close":true,"extra":1}`} {
		if _, err := decodeShellWriteArguments(raw); err == nil {
			t.Fatalf("decodeShellWriteArguments(%q) succeeded", raw)
		}
	}
}

func TestShellWriteHeader(t *testing.T) {
	for _, test := range []struct {
		input string
		close bool
		want  string
	}{
		{"2+3\n", false, "$< 2+3"},
		{"a\nb", false, "$< a\n  b"},
		{"", true, "$< EOF"},
		{"quit\n", true, "$< quit\n  EOF"},
	} {
		if got := shellWriteHeader(test.input, test.close); got != test.want {
			t.Errorf("shellWriteHeader(%q, %v) = %q, want %q", test.input, test.close, got, test.want)
		}
	}
}

func TestRunToolsUsesCompletionOrder(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "chat")
	c := conversation{path: path}
	openConversation(t, &c)
	// Recording the fast result creates chat.live and releases the slow command.
	calls := []toolCall{
		{
			ID:   "slow",
			Type: "function",
			Function: toolFunction{
				Name:      "shell",
				Arguments: `{"command":"while [ ! -e chat.live ]; do :; done\nprintf slow\n"}`,
			},
		},
		{
			ID:   "fast",
			Type: "function",
			Function: toolFunction{
				Name:      "shell",
				Arguments: `{"command":"printf fast"}`,
			},
		},
	}

	var display bytes.Buffer
	_, results, err := c.runTools(calls, "tool_calls", &display)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ToolCallID != "fast" || results[1].ToolCallID != "slow" {
		t.Fatalf("results = %#v, want fast then slow", results)
	}
	if results[0].Content != "fast\nexit status: 0" || results[1].Content != "slow\nexit status: 0" {
		t.Fatalf("result contents = %#v", results)
	}

	wantDisplay := "$ printf fast\n  " + path + ".live:2\n\n" +
		"$ while [ ! -e chat.live ]; do :; done\n  printf slow\n  " + path + ".live:7\n\n"
	if display.String() != wantDisplay {
		t.Fatalf("display = %q, want %q", display.String(), wantDisplay)
	}
	data, err := os.ReadFile(path + ".live")
	if err != nil {
		t.Fatal(err)
	}
	wantLive := "$ printf fast\nfast\nexit status: 0\n\n" +
		"$ while [ ! -e chat.live ]; do :; done\n  printf slow\nslow\nexit status: 0\n\n"
	if string(data) != wantLive {
		t.Fatalf("live transcript = %q, want %q", data, wantLive)
	}
}

func TestRunToolsReportsWaitBeforeReturn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat")
	process := &shellProcess{done: make(chan struct{})}
	c := conversation{path: path, commands: shellCommands{
		active: map[string]*shellProcess{"slow": process},
	}}
	openConversation(t, &c)
	call := toolCall{
		ID: "wait", Type: "function",
		Function: toolFunction{Name: shellWaitTool.name, Arguments: `{"command_ids":["slow"]}`},
	}
	display := &noticeWriter{notices: make(chan struct{}, 2)}
	announced := make(chan bool, 1)
	go func() {
		beforeTimeout := false
		select {
		case <-display.notices:
			beforeTimeout = true
		case <-time.After(time.Second):
		}
		process.mu.Lock()
		process.exited = true
		process.mu.Unlock()
		close(process.done)
		announced <- beforeTimeout
	}()

	_, _, err := c.runTools([]toolCall{call}, "tool_calls", display)
	wasAnnounced := <-announced
	if err != nil {
		t.Fatal(err)
	}
	if !wasAnnounced {
		t.Fatal("wait status was not written before the command completed")
	}

	wantDisplay := "… Waiting on 1 command\n  " + path + ".live:2\n\n" +
		"… Wait returned\n  " + path + ".live:5\n\n"
	if display.String() != wantDisplay {
		t.Fatalf("display = %q, want %q", display.String(), wantDisplay)
	}
	data, err := os.ReadFile(path + ".live")
	if err != nil {
		t.Fatal(err)
	}
	wantLive := "… Waiting on 1 command\nslow\n\n" +
		"… Wait returned\ncommand completed: slow\nexit status: 0\n\n"
	if string(data) != wantLive {
		t.Fatalf("live transcript = %q, want %q", data, wantLive)
	}
}

func TestHostShellApproval(t *testing.T) {
	for _, test := range []struct {
		answer string
		ran    bool
	}{
		{"y\n", true},
		{"n\n", false},
	} {
		t.Run(strings.TrimSpace(test.answer), func(t *testing.T) {
			dir := t.TempDir()
			workspace := filepath.Join(dir, "workspace")
			if err := os.Mkdir(workspace, 0755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(workspace)
			marker := filepath.Join(dir, "ran")
			c := conversation{
				path:      filepath.Join(workspace, "chat"),
				hostInput: bufio.NewReader(strings.NewReader(test.answer)),
			}
			openConversation(t, &c)
			call := toolCall{
				ID:   "host",
				Type: "function",
				Function: toolFunction{
					Name:      hostShellTool.name,
					Arguments: `{"command":"touch ` + marker + `"}`,
				},
			}

			if _, _, err := c.runTools([]toolCall{call}, "tool_calls", io.Discard); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(marker)
			if test.ran && err != nil {
				t.Fatalf("approved command did not run: %v", err)
			}
			if !test.ran && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected command ran: %v", err)
			}
		})
	}
}

func TestRejectedToolLiveRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat")
	c := conversation{path: path}
	openConversation(t, &c)
	call := toolCall{
		ID:   "bad",
		Type: "function",
		Function: toolFunction{
			Name:      "shell",
			Arguments: `{"command":`,
		},
	}

	var display bytes.Buffer
	if err := c.recordRejected(call, "invalid shell arguments", &display); err != nil {
		t.Fatal(err)
	}
	wantDisplay := "# rejected tool call\n  " + path + ".live:2\n\n"
	if display.String() != wantDisplay {
		t.Fatalf("display = %q, want %q", display.String(), wantDisplay)
	}
	data, err := os.ReadFile(path + ".live")
	if err != nil {
		t.Fatal(err)
	}
	wantLive := "# rejected tool call\n" +
		`{"id":"bad","type":"function","function":{"name":"shell","arguments":"{\"command\":"}}` +
		"\nerror: invalid shell arguments\n\n"
	if string(data) != wantLive {
		t.Fatalf("live transcript = %q, want %q", data, wantLive)
	}
}

func TestDisabledToolIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat")
	marker := filepath.Join(dir, "marker")
	c := conversation{path: path, noTools: true}
	openConversation(t, &c)
	call := toolCall{
		ID:   "disabled",
		Type: "function",
		Function: toolFunction{
			Name:      "shell",
			Arguments: `{"command":"touch ` + marker + `"}`,
		},
	}

	updated, results, err := c.runTools([]toolCall{call}, "tool_calls", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 1 || !updated[0].Rejected || len(results) != 1 || !results[0].Transient || results[0].Content != "tool error: tools are disabled" {
		t.Fatalf("calls = %#v, results = %#v", updated, results)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("disabled command ran: %v", err)
	}
}

func TestRunToolsRejectsInvalidIDsBeforeExecution(t *testing.T) {
	for _, ids := range [][]string{{""}, {"same", "same"}} {
		dir := t.TempDir()
		marker := filepath.Join(dir, "ran")
		calls := make([]toolCall, len(ids))
		for i, id := range ids {
			calls[i] = toolCall{
				ID:       id,
				Type:     "function",
				Function: toolFunction{Name: "shell", Arguments: `{"command":"touch ` + marker + `"}`},
			}
		}
		c := conversation{path: filepath.Join(dir, "chat")}
		openConversation(t, &c)
		if _, _, err := c.runTools(calls, "tool_calls", io.Discard); err == nil {
			t.Fatalf("ids %q were accepted", ids)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("ids %q allowed execution: %v", ids, err)
		}
	}
}
