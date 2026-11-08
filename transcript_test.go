package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTranscriptRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"blank lines", "\nhello\n\n"},
		{"dot lines", ".\n..\n...\n.gitignore\n..x"},
		{"unicode and carriage return", "héllo\rthere"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := []message{
				{Role: "user", Content: tt.content},
				{Role: "assistant", Content: tt.content},
			}
			data, err := formatTranscript(want)
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseTranscript(data)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip = %#v, want %#v", got, want)
			}
		})
	}
}

func TestTranscriptFormat(t *testing.T) {
	messages := []message{
		{Role: "user", Content: "hello\n"},
		{Role: "assistant", Content: ".\n.gitignore", Reasoning: "think"},
	}
	want := "user:\nhello\n\n.\nassistant:\n..\n.gitignore\n.\n"
	got, err := formatTranscript(messages)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("format = %q, want %q", got, want)
	}
}

func TestTranscriptSummaryAndCompaction(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		messages []message
	}{
		{
			name: "summary only",
			data: "summary:\ncheckpoint\n.\n",
			messages: []message{
				{Role: "user", Content: "checkpoint", Summary: true},
			},
		},
		{
			name: "tool continuation closed by compaction",
			data: "summary:\ncheckpoint\n.\nshell tool:\necho old\n.\noutput:\nold\n.\ncompaction:\n",
			messages: []message{
				{Role: "user", Content: "checkpoint", Summary: true},
				{
					Role: "assistant",
					ToolCalls: []toolCall{{
						ID:   "transcript-tool-1",
						Type: "function",
						Function: toolFunction{
							Name:      "shell",
							Arguments: `{"command":"echo old"}`,
						},
					}},
				},
				{Role: "tool", Content: "old", ToolCallID: "transcript-tool-1"},
				{Compaction: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTranscript([]byte(tt.data))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.messages) {
				t.Fatalf("parse = %#v, want %#v", got, tt.messages)
			}

			formatted, err := formatTranscript(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(formatted) != tt.data {
				t.Fatalf("format = %q, want %q", formatted, tt.data)
			}
		})
	}
}

func TestTranscriptToolReplay(t *testing.T) {
	data := []byte(`user:
inspect
.
shell tool:
echo found
.
output:
found
exit status: 0
.
assistant:
It was found.
.
`)

	messages, err := parseTranscript(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %#v", messages)
	}
	callMessage := messages[1]
	if callMessage.Role != "assistant" || len(callMessage.ToolCalls) != 1 {
		t.Fatalf("tool call message = %#v", callMessage)
	}
	call := callMessage.ToolCalls[0]
	if call.ID == "" || call.Type != "function" || call.Function.Name != "shell" || call.Function.Arguments != `{"command":"echo found"}` {
		t.Errorf("replayed call = %#v", call)
	}
	if !reflect.DeepEqual(messages[2], message{Role: "tool", Content: "found\nexit status: 0", ToolCallID: call.ID}) {
		t.Errorf("tool result = %#v", messages[2])
	}

	formatted, err := formatTranscript(messages)
	if err != nil {
		t.Fatal(err)
	}
	if string(formatted) != string(data) {
		t.Fatalf("reformatted transcript = %q, want %q", formatted, data)
	}
}

func TestTranscriptInteractiveToolReplay(t *testing.T) {
	data := []byte(`user:
interact
.
shell write tool:
{ "command_id":"launch", "input":"hello\n", "close":true }
.
output:
queued 6 bytes
stdin close queued
.
shell read tool:
{"command_id":"launch"}
.
output:
hello
exit status: 0
.
assistant:
done
.
`)

	messages, err := parseTranscript(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 6 {
		t.Fatalf("messages = %#v", messages)
	}
	for i, want := range []struct {
		name      string
		arguments string
		output    string
	}{
		{"shell_write", `{ "command_id":"launch", "input":"hello\n", "close":true }`, "queued 6 bytes\nstdin close queued"},
		{"shell_read", `{"command_id":"launch"}`, "hello\nexit status: 0"},
	} {
		callMessage := messages[1+i*2]
		result := messages[2+i*2]
		if len(callMessage.ToolCalls) != 1 {
			t.Fatalf("tool call message %d = %#v", i, callMessage)
		}
		call := callMessage.ToolCalls[0]
		if call.Function.Name != want.name || call.Function.Arguments != want.arguments {
			t.Errorf("tool call %d = %#v", i, call)
		}
		if result.Content != want.output || result.ToolCallID != call.ID {
			t.Errorf("tool result %d = %#v", i, result)
		}
	}

	formatted, err := formatTranscript(messages)
	if err != nil {
		t.Fatal(err)
	}
	if string(formatted) != string(data) {
		t.Fatalf("reformatted transcript = %q, want %q", formatted, data)
	}
}

func TestTranscriptFormatsToolsInCompletionOrder(t *testing.T) {
	messages := []message{
		{Role: "user", Content: "run both"},
		{
			Role: "assistant",
			ToolCalls: []toolCall{
				{ID: "slow", Type: "function", Function: toolFunction{Name: "shell", Arguments: `{"command":"slow"}`}},
				{ID: "fast", Type: "function", Function: toolFunction{Name: "shell", Arguments: `{"command":"fast"}`}},
			},
		},
		{Role: "tool", Content: "F", ToolCallID: "fast"},
		{Role: "tool", Content: "S", ToolCallID: "slow"},
		{Role: "assistant", Content: "done"},
	}
	want := `user:
run both
.
shell tool:
fast
.
output:
F
.
shell tool:
slow
.
output:
S
.
assistant:
done
.
`

	got, err := formatTranscript(messages)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("format = %q, want %q", got, want)
	}
}

func TestTranscriptOmitsRejectedToolCall(t *testing.T) {
	messages := []message{
		{Role: "user", Content: "try"},
		{
			Role: "assistant",
			ToolCalls: []toolCall{{
				ID:       "bad",
				Rejected: true,
			}},
		},
		{Role: "tool", ToolCallID: "bad", Transient: true},
		{Role: "assistant", Content: "I could not run it."},
	}
	if _, err := formatTranscript(messages[:3]); err == nil {
		t.Fatal("rejected tool call was accepted without a final response")
	}
	want := "user:\ntry\n.\nassistant:\nI could not run it.\n.\n"
	got, err := formatTranscript(messages)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("format = %q, want %q", got, want)
	}
}

func TestTranscriptRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"bad utf8", []byte("user:\n\xff\n.\n")},
		{"no final lf", []byte("user:\n\n.")},
		{"wrong first role", []byte("assistant:\n\n.\n")},
		{"reasoning unsupported", []byte("user:\nu\n.\nreasoning:\nr\n.\nassistant:\na\n.\n")},
		{"missing framing line", []byte("user:\n.\n")},
		{"incomplete pair", []byte("user:\n\n.\n")},
		{"crlf structure", []byte("user:\r\n\r\n.\r\n")},
		{"invalid interactive tool arguments", []byte("user:\nu\n.\nshell read tool:\nnope\n.\noutput:\nx\n.\nassistant:\na\n.\n")},
		{"summary after first record", []byte("user:\nu\n.\nassistant:\na\n.\nsummary:\ns\n.\n")},
		{"framed compaction", []byte("compaction:\nbody\n.\n")},
		{"nonterminal compaction", []byte("compaction:\nuser:\nu\n.\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseTranscript(tt.data)
			if err == nil {
				t.Fatal("malformed transcript was accepted")
			}
		})
	}
}

func TestReplaceTranscriptPreservesModeAndStaleTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat")
	old := []byte("user:\nold\n.\nassistant:\nreply\n.\n")
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}

	messages := []message{{Role: "user", Content: "new"}, {Role: "assistant", Content: "answer"}}
	if err := replaceTranscriptPath(path, messages); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"---writing", []byte("recover me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := replaceTranscriptPath(path, messages); err == nil {
		t.Fatal("replace succeeded with stale temporary file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(current) {
		t.Fatal("stale temporary file changed the transcript")
	}
	temp, err := os.ReadFile(path + "---writing")
	if err != nil {
		t.Fatal(err)
	}
	if string(temp) != "recover me" {
		t.Fatal("stale temporary file was changed")
	}
}

func TestReplaceTranscriptReturnsAssistantBodyLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	messages := []message{
		{Role: "user", Content: "prompt"},
		{Role: "assistant", Content: ".\n\nlast\n\n"},
	}
	line, err := replaceTranscript(root, "chat", path, messages)
	if err != nil {
		t.Fatal(err)
	}
	if line != 5 {
		t.Fatalf("assistant line = %d, want 5", line)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	if lines[line-1] != ".." {
		t.Fatalf("addressed line = %q, want escaped assistant body", lines[line-1])
	}
}

func TestResolveTranscriptSymlink(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realDir, "chat")
	if err := os.WriteFile(target, nil, 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "chat")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	resolved, err := resolveTranscriptPath(link)
	if err != nil {
		t.Fatal(err)
	}
	messages := []message{{Role: "user", Content: "u"}, {Role: "assistant", Content: "a"}}
	if err := replaceTranscriptPath(resolved, messages); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("transcript symlink was replaced: info=%v err=%v", info, err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseTranscript(data)
	if err != nil || !reflect.DeepEqual(got, messages) {
		t.Fatalf("target = %#v, %v; want %#v", got, err, messages)
	}
}

func replaceTranscriptPath(path string, messages []message) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer root.Close()
	_, err = replaceTranscript(root, filepath.Base(path), path, messages)
	return err
}

func TestResolveMissingTranscriptThroughSymlinkedParent(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "linked")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	got, err := resolveTranscriptPath(filepath.Join(linkDir, "chat"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realDir, "chat")
	if got != want {
		t.Fatalf("resolved = %q, want %q", got, want)
	}

	if _, err := resolveTranscriptPath(filepath.Join(dir, "missing", "chat")); err == nil {
		t.Fatal("missing explicit parent was accepted")
	}
}
