package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestStreamCompletion(t *testing.T) {
	wantMessages := []message{
		{Role: "user", Content: "before"},
		{Role: "assistant", Content: "reply", Reasoning: "considered"},
		{Role: "user", Content: "hello"},
	}
	wantModel := "test-model"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["messages"] == nil || string(body["model"]) != `"`+wantModel+`"` || string(body["stream"]) != "true" || string(body["reasoning_effort"]) != `"xhigh"` || string(body["parallel_tool_calls"]) != "true" {
			t.Errorf("request body fields = %v", body)
		}
		if tools := body["tools"]; !bytes.Contains(tools, []byte(`"name":"shell"`)) ||
			!bytes.Contains(tools, []byte(`"name":"shell_read"`)) ||
			!bytes.Contains(tools, []byte(`"name":"shell_write"`)) ||
			!bytes.Contains(tools, []byte(`"strict":true`)) {
			t.Errorf("tools = %s, want strict shell tool", tools)
		}
		if !bytes.Contains(body["messages"], []byte(`"reasoning_content":"considered"`)) {
			t.Errorf("messages omit reasoning_content: %s", body["messages"])
		}
		var gotMessages []message
		if err := json.Unmarshal(body["messages"], &gotMessages); err != nil {
			t.Error(err)
		} else if !reflect.DeepEqual(gotMessages, wantMessages) {
			t.Errorf("messages = %#v, want %#v", gotMessages, wantMessages)
		}

		io.WriteString(w, ": ping\r\n\r\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"one\\n\"}}]}\r\n\r\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"two\"}}]}\r\n\r\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_text\":\"\\nthree\"}}]}\r\n\r\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\r\n\r\n")
		io.WriteString(w, "data: {\"choices\":[\r\n")
		io.WriteString(w, "data: {\"delta\":{\"content\":\"lo\"}}]}\r\n\r\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"shell","arguments":"{\"command\":\"printf "}}]}}]}`+"\r\n\r\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"shell","arguments":"{\"command\":\"printf "}}]}}]}`+"\r\n\r\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"b\"}"}},{"index":0,"function":{"arguments":"a\"}"}}]}}]}`+"\r\n\r\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\r\n\r\n")
		io.WriteString(w, "data: [DONE]\r\n\r\n")
	}))
	t.Cleanup(server.Close)

	var out, reasoningOut bytes.Buffer
	got, err := streamCompletion(server.URL+"/", wantModel, "xhigh", wantMessages, availableTools, &out, &reasoningOut, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got.Message.Content != "hello" || got.Message.Reasoning != "one\ntwo\nthree" || out.String() != "hello" {
		t.Fatalf("response = %#v, stdout = %q", got, out.String())
	}
	wantCalls := []toolCall{
		{ID: "call_a", Type: "function", Function: toolFunction{Name: "shell", Arguments: `{"command":"printf a"}`}},
		{ID: "call_b", Type: "function", Function: toolFunction{Name: "shell", Arguments: `{"command":"printf b"}`}},
	}
	if !reflect.DeepEqual(got.Message.ToolCalls, wantCalls) || got.FinishReason != "stop" {
		t.Fatalf("tool completion = %#v, want calls %#v and finish reason stop", got, wantCalls)
	}
	if reasoningOut.String() != "~ one\ntwo\nthree\n\n" {
		t.Fatalf("stderr = %q", reasoningOut.String())
	}
}

func TestStreamCompletionToolsDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body["tools"] != nil || body["parallel_tool_calls"] != nil {
			t.Errorf("disabled request advertises tools: %v", body)
		}
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":"length"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	got, err := streamCompletion(server.URL, "test-model", "", []message{{Role: "user", Content: "x"}}, nil, io.Discard, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got.FinishReason != "length" || len(got.Message.ToolCalls) != 1 || got.Message.ToolCalls[0].ID != "call_1" {
		t.Fatalf("completion = %#v", got)
	}
}

func TestStreamCompletionFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantOutput string
		wantError  string
	}{
		{"missing done", 200, "data: {\"choices\":[{\"delta\":{\"content\":\"part\"}}]}\n\n", "part", "before [DONE]"},
		{"malformed json", 200, "data: {nope}\n\n", "", "decode server event"},
		{"invalid utf8", 200, "data: \xff\n\n", "", "not valid UTF-8"},
		{"server event error", 200, "data: {\"error\":{\"message\":\"bad\"}}\n\n", "", "server error"},
		{"negative tool index", 200, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":-1}]}}]}\n\n", "", "negative tool call index"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)

			var out bytes.Buffer
			_, err := streamCompletion(server.URL, "test-model", "", []message{{Role: "user", Content: "x"}}, nil, &out, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantError)
			}
			if out.String() != tt.wantOutput {
				t.Fatalf("stdout = %q, want %q", out.String(), tt.wantOutput)
			}
		})
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("broken output") }

func TestStreamCompletionOutputFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)

	_, err := streamCompletion(server.URL, "test-model", "", []message{{Role: "user", Content: "x"}}, nil, errorWriter{}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "write response") {
		t.Fatalf("error = %v, want output failure", err)
	}
}
