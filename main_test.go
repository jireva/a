package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPipedAndArgumentPrompt(t *testing.T) {
	var requests [][]message
	server := chatServer(t, func(messages []message) string {
		requests = append(requests, messages)
		return "answer"
	})

	dir := t.TempDir()
	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	a := testApp(server, strings.NewReader("from pipe\n"), &stdout, &stderr)
	if code := a.run([]string{"from", "arguments"}); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if stdout.String() != "answer" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	wantPath := filepath.Join(dir, ".a", "transcripts", "123")
	wantStderr := "\n  " + wantPath + ":9\n\n"
	if stderr.String() != wantStderr {
		t.Fatalf("stderr = %q, want %q", stderr.String(), wantStderr)
	}
	if len(requests) != 1 || len(requests[0]) != 1 || requests[0][0].Content != "from pipe\n\n\n\nfrom arguments" {
		t.Fatalf("request messages = %#v", requests)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := parseTranscript(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Content != requests[0][0].Content || stored[1].Content != "answer" {
		t.Fatalf("stored messages = %#v", stored)
	}
}

func TestPipedNoOpDoesNotInspectTranscript(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("not a transcript"), 0644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := app{
		stdin:   strings.NewReader(""),
		stdout:  &stdout,
		stderr:  &stderr,
		started: 123,
		server:  "http://unused.invalid",
		model:   "test-model",
	}
	if code := a.run([]string{"-t", bad}); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestInvalidUTF8PromptMakesNoRequest(t *testing.T) {
	called := false
	server := chatServer(t, func([]message) string {
		called = true
		return "unexpected"
	})

	dir := t.TempDir()
	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	a := testApp(server, bytes.NewReader([]byte{0xff}), &stdout, &stderr)
	if code := a.run(nil); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if called || !strings.Contains(stderr.String(), "not valid UTF-8") {
		t.Fatalf("called = %v, stderr = %q", called, stderr.String())
	}
	path := filepath.Join(dir, ".a", "transcripts", "123")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid prompt created transcript: %v", err)
	}
}

func TestExplicitTranscriptPrintsAddress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat")
	original := []message{{Role: "user", Content: "old"}, {Role: "assistant", Content: "reply"}}
	data, err := formatTranscript(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	server := chatServer(t, func(messages []message) string {
		if len(messages) != 3 || !reflect.DeepEqual(messages[0], original[0]) || !reflect.DeepEqual(messages[1], original[1]) || messages[2].Content != "next" {
			t.Errorf("messages = %#v", messages)
		}
		return "done"
	})

	var stdout, stderr, terminal bytes.Buffer
	a := testApp(server, strings.NewReader(""), io.MultiWriter(&stdout, &terminal), io.MultiWriter(&stderr, &terminal))
	a.stdinTerminal = true
	if code := a.run([]string{"-t", path, "next"}); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	wantStderr := "\n  " + path + ":11\n\n"
	if stdout.String() != "done" || stderr.String() != wantStderr {
		t.Fatalf("stdout = %q, stderr = %q, want stderr %q", stdout.String(), stderr.String(), wantStderr)
	}
	wantTerminal := "done\n  " + path + ":11\n\n"
	if terminal.String() != wantTerminal {
		t.Fatalf("terminal = %q, want %q", terminal.String(), wantTerminal)
	}
}

func TestFlags(t *testing.T) {
	a := app{
		stdin:  strings.NewReader(""),
		stdout: io.Discard,
		stderr: io.Discard,
	}
	if code := a.run([]string{"-e", "custom", "-n"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if a.effort != "custom" {
		t.Fatalf("effort = %q, want custom", a.effort)
	}
	if !a.noTools {
		t.Fatal("-n did not disable tools")
	}
}

func TestREPL(t *testing.T) {
	var requests [][]message
	server := chatServer(t, func(messages []message) string {
		requests = append(requests, messages)
		return "R"
	})

	dir := t.TempDir()
	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	a := testApp(server, strings.NewReader("\nfirst\\\ncontinued\nsecond\n"), &stdout, &stderr)
	a.stdinTerminal = true
	if code := a.run(nil); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if stdout.String() != "RR" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if len(requests) != 2 || len(requests[0]) != 1 || len(requests[1]) != 3 {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[1][0].Content != "first\ncontinued" || requests[1][1].Content != "R" || requests[1][2].Content != "second" {
		t.Fatalf("second request = %#v", requests[1])
	}
	wantPath := filepath.Join(dir, ".a", "transcripts", "123")
	wantStderr := "> >   \n\n  " + wantPath + ":6\n\n> \n\n  " +
		wantPath + ":12\n\n> \n"
	if stderr.String() != wantStderr {
		t.Fatalf("stderr = %q, want %q", stderr.String(), wantStderr)
	}
}

func TestFailedStreamIsNotCommittedOrAnnounced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"think\"}}]}\n\n")
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	a := testApp(server, strings.NewReader("prompt"), &stdout, &stderr)
	if code := a.run(nil); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "~ think\n\n… ") || !strings.Contains(stderr.String(), "before [DONE]") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	path := filepath.Join(dir, ".a", "transcripts", "123")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed turn created transcript: %v", err)
	}
	if strings.Contains(stderr.String(), path) {
		t.Fatalf("failed turn announced transcript: %q", stderr.String())
	}
}

func TestToolTurn(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "workspace")
	shared := filepath.Join(parent, "shared")
	for _, path := range []string{dir, shared} {
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SHARED", shared)
	t.Chdir(dir)
	path := filepath.Join(dir, "chat")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests++
		if requests == 1 {
			io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"reasoning":"plan","content":"Let me check.","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"printf tool-output > \\\"$SHARED/result\\\"; cat \\\"$SHARED/result\\\"\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		if len(request.Messages) != 3 || request.Messages[1].Reasoning != "plan" || request.Messages[2].Role != "tool" || request.Messages[2].ToolCallID != "call_1" || request.Messages[2].Content != "tool-output\nexit status: 0" {
			t.Errorf("second request = %#v", request.Messages)
		}
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"reasoning":"finish","content":"Done.\n"},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	var stdout, stderr, terminal bytes.Buffer
	a := testApp(server, strings.NewReader("inspect"), io.MultiWriter(&stdout, &terminal), io.MultiWriter(&stderr, &terminal))
	if code := a.run([]string{"-rw", shared, "-t", path}); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	for _, message := range a.conversation.messages {
		if message.Reasoning != "" {
			t.Fatalf("committed reasoning = %q", message.Reasoning)
		}
	}
	if stdout.String() != "Let me check.Done.\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	wantStderr := "~ plan\n\n\n\n$ printf tool-output > \"$SHARED/result\"; cat \"$SHARED/result\"\n  " + path +
		".live:2\n\n~ finish\n\n  " + path + ":15\n\n"
	if stderr.String() != wantStderr {
		t.Fatalf("stderr = %q, want %q", stderr.String(), wantStderr)
	}
	wantTerminal := "~ plan\n\nLet me check.\n\n" +
		"$ printf tool-output > \"$SHARED/result\"; cat \"$SHARED/result\"\n  " + path + ".live:2\n\n" +
		"~ finish\n\nDone.\n  " + path + ":15\n\n"
	if terminal.String() != wantTerminal {
		t.Fatalf("terminal = %q, want %q", terminal.String(), wantTerminal)
	}
	if _, err := os.Stat(path + ".live"); !os.IsNotExist(err) {
		t.Fatalf("live transcript remains after exit: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantTranscript := "user:\ninspect\n.\nassistant:\nLet me check.\n.\nshell tool:\nprintf tool-output > \"$SHARED/result\"; cat \"$SHARED/result\"\n.\noutput:\ntool-output\nexit status: 0\n.\nassistant:\nDone.\n\n.\n"
	if string(data) != wantTranscript {
		t.Fatalf("transcript = %q, want %q", data, wantTranscript)
	}
	if data, err := os.ReadFile(filepath.Join(shared, "result")); err != nil || string(data) != "tool-output" {
		t.Fatalf("shared file = %q, %v", data, err)
	}
}

func TestCompactionRotatesTranscript(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	oldPath := filepath.Join(dir, "chat")
	original := []message{
		{Role: "user", Content: strings.Repeat("x", maxRequestBytes)},
		{Role: "assistant", Content: "old answer"},
	}
	oldData, err := formatTranscript(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, oldData, 0644); err != nil {
		t.Fatal(err)
	}
	wantSummary := "You are continuing a session whose earlier conversation was compacted into the checkpoint below.\n\n" +
		"The full transcript before compaction is:\n" + oldPath +
		"\n\nRead that transcript if more detail is needed.\n\nCheckpoint:\ncheckpoint"

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeTestChatRequest(t, r)
		requests++
		if request.Model != "test-model" || request.ReasoningEffort != "high" {
			t.Errorf("request %d model/effort = %q/%q", requests, request.Model, request.ReasoningEffort)
		}
		switch requests {
		case 1:
			if len(request.Tools) != 0 || request.ParallelToolCalls {
				t.Errorf("compaction request advertises tools")
			}
			if len(request.Messages) != 3 || request.Messages[0].Content != original[0].Content || request.Messages[1].Content != "old answer" || request.Messages[2].Content != compactionPrompt {
				t.Errorf("compaction request does not contain exactly the old history and prompt")
			}
			writeStopCompletion(w, "checkpoint")
		case 2:
			if len(request.Tools) == 0 || !request.ParallelToolCalls {
				t.Errorf("continuation request omits tools")
			}
			if len(request.Messages) != 2 || request.Messages[0].Content != wantSummary || request.Messages[1].Content != "next" {
				t.Errorf("continuation request does not contain the summary and exact pending prompt")
			}
			writeStopCompletion(w, "answer")
		default:
			t.Errorf("unexpected request %d", requests)
			writeStopCompletion(w, "unexpected")
		}
	}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	a := testApp(server, strings.NewReader(""), &stdout, &stderr)
	a.stdinTerminal = true
	if code := a.run([]string{"-e", "high", "-t", oldPath, "next"}); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}

	closed, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	wantClosed := append(append([]byte(nil), oldData...), []byte("compaction:\n")...)
	if !bytes.Equal(closed, wantClosed) {
		t.Fatal("old transcript was not closed with a bare compaction sentinel")
	}

	newPath := filepath.Join(dir, ".a", "transcripts", "123")
	stored, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	newMessages, err := parseTranscript(stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(newMessages) != 3 || !newMessages[0].Summary || newMessages[0].Content != wantSummary || newMessages[1].Content != "next" || newMessages[2].Content != "answer" {
		t.Fatalf("successor messages = %#v", newMessages)
	}
	wantStderr := "… Compacting transcript\n  " + oldPath + "\n\n" +
		"… Continuing in transcript\n  " + newPath + "\n\n\n  " + newPath + ":16\n\n"
	if stderr.String() != wantStderr {
		t.Fatalf("stderr = %q, want %q", stderr.String(), wantStderr)
	}
}

func TestCompactionDuringToolTurn(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	transcriptDir := filepath.Join(dir, ".a", "transcripts")
	if err := os.MkdirAll(transcriptDir, 0755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(transcriptDir, "123")
	prompt := strings.Repeat("p", 394500)

	command := `head -c 4000 /dev/zero | tr '\0' x`
	argumentData, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	arguments := string(argumentData)
	call := toolCall{ID: "call_big", Type: "function", Function: toolFunction{Name: "shell", Arguments: arguments}}
	toolResult := strings.Repeat("x", 4000) + "\nexit status: 0"

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeTestChatRequest(t, r)
		requests++
		switch requests {
		case 1:
			if len(request.Messages) != 1 || request.Messages[0].Content != prompt || len(request.Tools) == 0 {
				t.Errorf("initial request is missing the prompt or tools")
			}
			encodedArguments, _ := json.Marshal(arguments)
			fmt.Fprintf(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_big","type":"function","function":{"name":"shell","arguments":%s}}]},"finish_reason":"tool_calls"}]}`+"\n\n", encodedArguments)
			io.WriteString(w, "data: [DONE]\n\n")
		case 2:
			if len(request.Tools) != 0 || request.ParallelToolCalls {
				t.Errorf("compaction request advertises tools")
			}
			if len(request.Messages) != 2 || request.Messages[0].Content != prompt || request.Messages[1].Content != compactionPrompt {
				t.Errorf("mid-tool compaction request has the wrong boundary")
			}
			writeStopCompletion(w, "tool checkpoint")
		case 3:
			if len(request.Messages) != 3 || len(request.Tools) == 0 {
				t.Errorf("tool continuation request has the wrong shape")
			} else {
				gotCall := request.Messages[1]
				gotResult := request.Messages[2]
				if gotCall.Role != "assistant" || len(gotCall.ToolCalls) != 1 || !reflect.DeepEqual(gotCall.ToolCalls[0], call) {
					t.Errorf("continuation changed the tool call")
				}
				if gotResult.Role != "tool" || gotResult.ToolCallID != call.ID || gotResult.Content != toolResult {
					t.Errorf("continuation changed the tool result")
				}
			}
			writeStopCompletion(w, "Done.")
		default:
			t.Errorf("unexpected request %d", requests)
			writeStopCompletion(w, "unexpected")
		}
	}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	a := testApp(server, strings.NewReader(""), &stdout, &stderr)
	a.stdinTerminal = true
	if code := a.run([]string{"-t", oldPath, prompt}); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}

	closed, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	closedMessages, err := parseTranscript(closed)
	if err != nil {
		t.Fatal(err)
	}
	if len(closedMessages) != 4 || closedMessages[2].Content != toolResult || !closedMessages[3].Compaction {
		t.Fatal("old transcript does not end with the exact tool result and compaction sentinel")
	}
	newPath := filepath.Join(transcriptDir, "124")
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("successor transcript: %v", err)
	}
}

func TestIneffectiveCompactionDoesNotRotate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	oldPath := filepath.Join(dir, "chat")
	original := []message{
		{Role: "user", Content: strings.Repeat("x", maxRequestBytes)},
		{Role: "assistant", Content: "old answer"},
	}
	oldData, err := formatTranscript(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, oldData, 0644); err != nil {
		t.Fatal(err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeStopCompletion(w, strings.Repeat("s", maxRequestBytes))
	}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	a := testApp(server, strings.NewReader(""), &stdout, &stderr)
	a.stdinTerminal = true
	if code := a.run([]string{"-t", oldPath, "next"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	gotOld, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotOld, oldData) {
		t.Fatal("failed compaction changed the old transcript")
	}
	newPath := filepath.Join(dir, ".a", "transcripts", "123")
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("failed compaction created successor: %v", err)
	}
	if strings.Contains(stderr.String(), "Continuing in transcript") || !strings.Contains(stderr.String(), "checkpoint does not reduce the request below the context limit") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCompactedTranscriptBranches(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	oldPath := filepath.Join(dir, "chat")
	closed := []byte("user:\nold\n.\nassistant:\nanswer\n.\ncompaction:\n")
	if err := os.WriteFile(oldPath, closed, 0644); err != nil {
		t.Fatal(err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := decodeTestChatRequest(t, r)
		requests++
		switch requests {
		case 1:
			if len(request.Messages) != 3 || request.Messages[2].Content != compactionPrompt {
				t.Errorf("branch compaction request = %#v", request.Messages)
			}
			writeStopCompletion(w, "branch checkpoint")
		case 2:
			if len(request.Messages) != 2 || !strings.Contains(request.Messages[0].Content, "Checkpoint:\nbranch checkpoint") || request.Messages[1].Content != "branch" {
				t.Errorf("branch continuation request = %#v", request.Messages)
			}
			writeStopCompletion(w, "branched")
		default:
			t.Errorf("unexpected request %d", requests)
			writeStopCompletion(w, "unexpected")
		}
	}))
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer
	a := testApp(server, strings.NewReader(""), &stdout, &stderr)
	a.stdinTerminal = true
	if code := a.run([]string{"-t", oldPath, "branch"}); code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	gotOld, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotOld, closed) {
		t.Fatal("branching changed the compacted transcript")
	}
}

func testApp(server *httptest.Server, stdin io.Reader, stdout, stderr io.Writer) app {
	return app{
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		started: 123,
		server:  server.URL,
		model:   "test-model",
	}
}

func chatServer(t *testing.T, answer func([]message) string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		content, _ := json.Marshal(answer(request.Messages))
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", content)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

type testChatRequest struct {
	Messages          []message     `json:"messages"`
	Model             string        `json:"model"`
	ReasoningEffort   string        `json:"reasoning_effort"`
	Tools             []requestTool `json:"tools"`
	ParallelToolCalls bool          `json:"parallel_tool_calls"`
}

func decodeTestChatRequest(t *testing.T, r *http.Request) testChatRequest {
	t.Helper()
	var request testChatRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode request: %v", err)
	}
	return request
}

func writeStopCompletion(w io.Writer, content string) {
	encoded, _ := json.Marshal(content)
	fmt.Fprintf(w, `data: {"choices":[{"index":0,"delta":{"content":%s},"finish_reason":"stop"}]}`+"\n\n", encoded)
	io.WriteString(w, "data: [DONE]\n\n")
}
