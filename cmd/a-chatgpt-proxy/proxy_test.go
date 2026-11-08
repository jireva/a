package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type proxyTestToken string

func (t proxyTestToken) token(context.Context) (string, error) { return string(t), nil }

type proxyFailingResponseWriter struct {
	header   http.Header
	writeErr error
	flushErr error
	writes   int
}

func (w *proxyFailingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (*proxyFailingResponseWriter) WriteHeader(int) {}

func (w *proxyFailingResponseWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(data), nil
}

func (*proxyFailingResponseWriter) Flush()              {}
func (w *proxyFailingResponseWriter) FlushError() error { return w.flushErr }

func TestProxyListsModels(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		io.WriteString(w, `{"models":[{"slug":"hidden","visibility":"hide"},{"slug":"second","visibility":"list"},{"slug":"first","visibility":"list"}]}`)
	}))
	t.Cleanup(server.Close)

	p := newProxy(proxyTestToken("test-token"), io.Discard)
	p.modelsURL = server.URL + "/v1/models"
	p.client = server.Client()
	for range 2 {
		response := httptest.NewRecorder()
		p.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		assertProxyJSON(t, response.Body.Bytes(), `{"data":[{"id":"second"},{"id":"first"}]}`)
	}
	if requests != 2 {
		t.Fatalf("upstream requests = %d, want 2", requests)
	}

	response := httptest.NewRecorder()
	p.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/models", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST response: status %d, headers %v", response.Code, response.Header())
	}
}

func TestProxyConvertsRequestAndStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "request-ok")
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		assertProxyJSON(t, body, `{
			"model":"gpt-test",
			"input":[
				{"role":"user","content":"old"},
				{"role":"assistant","content":"answer"},
				{"role":"user","content":"new"}
			],
			"store":false,
			"stream":true,
			"reasoning":{"effort":"xhigh","summary":"auto"},
			"tools":[{"type":"namespace","name":"a","description":"Tools available through a.","tools":[{
				"type":"function","name":"shell","description":"Run a command.","strict":true,
				"parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}
			}]}],
			"parallel_tool_calls":true
		}`)

		proxyResponseEvents(w,
			`{"type":"keepalive"}`,
			`{"type":"response.metadata","metadata":{}}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":0,"delta":"**Assessing symlink handling in sandbox**"}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":1,"delta":"**Planning recursive symlink binding for sandbox**"}`,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"summary_index":1,"delta":"**Verifying bubblewrap bind destination behavior**"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"opaque","summary":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"hello"}`,
			`{"type":"keepalive"}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`,
			`{"type":"response.completed","response":{"status":"completed"}}`,
		)
	}))
	t.Cleanup(server.Close)

	p := newProxy(proxyTestToken("test-token"), io.Discard)
	p.upstream = server.URL
	p.client = server.Client()
	request := proxyTestRequest([]chatMessage{
		proxyMessage("user", "old"),
		{Role: "assistant", Content: proxyString("answer"), ReasoningContent: "discarded summary"},
		proxyMessage("user", "new"),
	})
	request.ReasoningEffort = "xhigh"
	response := proxyPost(t, p, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Request-Id") != "request-ok" {
		t.Fatalf("X-Request-Id = %q", response.Header().Get("X-Request-Id"))
	}
	data := proxySSEData(t, response.Body.String())
	if len(data) != 6 {
		t.Fatalf("events = %#v", data)
	}
	assertProxyJSON(t, []byte(data[0]), `{"choices":[{"index":0,"delta":{"reasoning_content":"**Assessing symlink handling in sandbox**"}}]}`)
	assertProxyJSON(t, []byte(data[1]), `{"choices":[{"index":0,"delta":{"reasoning_content":"\n  **Planning recursive symlink binding for sandbox**"}}]}`)
	assertProxyJSON(t, []byte(data[2]), `{"choices":[{"index":0,"delta":{"reasoning_content":"**Verifying bubblewrap bind destination behavior**"}}]}`)
	assertProxyJSON(t, []byte(data[3]), `{"choices":[{"index":0,"delta":{"content":"hello"}}]}`)
	assertProxyJSON(t, []byte(data[4]), `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if data[5] != "[DONE]" {
		t.Fatalf("last event = %q", data[5])
	}
}

func TestProxyPreservesToolLoopState(t *testing.T) {
	reasoning1 := `{"id":"rs_1","type":"reasoning","encrypted_content":"cipher-1","summary":[]}`
	callA := `{"id":"fc_a","type":"function_call","status":"completed","call_id":"call_a","namespace":"a","name":"shell","arguments":"{\"command\":\"a\"}"}`
	message1 := `{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"before tools"}]}`
	callB := `{"id":"fc_b","type":"function_call","status":"completed","call_id":"call_b","namespace":"a","name":"shell","arguments":"{\"command\":\"b\"}"}`
	reasoning2 := `{"id":"rs_2","type":"reasoning","encrypted_content":"cipher-2","summary":[]}`
	callC := `{"id":"fc_c","type":"function_call","status":"completed","call_id":"call_c","namespace":"a","name":"shell","arguments":"{\"command\":\"c\"}"}`

	round := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round++
		var request struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		switch round {
		case 1:
			assertProxyItems(t, request.Input, `{"role":"user","content":"work"}`)
			proxyResponseEvents(w,
				`{"type":"response.function_call_arguments.delta","item_id":"fc_b","output_index":3,"delta":"ignored"}`,
				proxyDoneEvent(3, callB),
				proxyDoneEvent(0, reasoning1),
				proxyDoneEvent(2, message1),
				proxyDoneEvent(1, callA),
				`{"type":"response.completed","response":{"status":"completed"}}`,
			)
		case 2:
			assertProxyItems(t, request.Input,
				`{"role":"user","content":"work"}`,
				reasoning1, callA, message1, callB,
				`{"type":"function_call_output","call_id":"call_b","output":"B"}`,
				`{"type":"function_call_output","call_id":"call_a","output":"A"}`,
			)
			proxyResponseEvents(w,
				proxyDoneEvent(1, callC),
				proxyDoneEvent(0, reasoning2),
				`{"type":"response.completed","response":{"status":"completed"}}`,
			)
		case 3:
			assertProxyItems(t, request.Input,
				`{"role":"user","content":"work"}`,
				reasoning1, callA, message1, callB,
				`{"type":"function_call_output","call_id":"call_b","output":"B"}`,
				`{"type":"function_call_output","call_id":"call_a","output":"A"}`,
				reasoning2, callC,
				`{"type":"function_call_output","call_id":"call_c","output":"C"}`,
			)
			proxyResponseEvents(w,
				`{"type":"response.output_text.delta","item_id":"msg_2","output_index":0,"content_index":0,"delta":"done"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_2","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
				`{"type":"response.completed","response":{"status":"completed"}}`,
			)
		default:
			t.Errorf("unexpected upstream round %d", round)
		}
	}))
	t.Cleanup(server.Close)

	p := newProxy(proxyTestToken("test-token"), io.Discard)
	p.upstream = server.URL
	p.client = server.Client()
	user := proxyMessage("user", "work")
	callAMessage := proxyToolCall("call_a", "a")
	callBMessage := proxyToolCall("call_b", "b")
	callCMessage := proxyToolCall("call_c", "c")

	first := proxyPost(t, p, proxyTestRequest([]chatMessage{user}))
	proxyAssertToolStream(t, first, []chatDeltaToolCall{
		proxyDeltaCall(0, callAMessage),
		proxyDeltaCall(1, callBMessage),
	})

	messages := []chatMessage{
		user,
		{Role: "assistant", Content: proxyString("before tools"), ToolCalls: []chatToolCall{callAMessage, callBMessage}},
		{Role: "tool", Content: proxyString("B"), ToolCallID: "call_b"},
		{Role: "tool", Content: proxyString("A"), ToolCallID: "call_a"},
	}
	second := proxyPost(t, p, proxyTestRequest(messages))
	proxyAssertToolStream(t, second, []chatDeltaToolCall{proxyDeltaCall(0, callCMessage)})

	messages = append(messages,
		chatMessage{Role: "assistant", Content: proxyString(""), ToolCalls: []chatToolCall{callCMessage}},
		chatMessage{Role: "tool", Content: proxyString("C"), ToolCallID: "call_c"},
	)
	third := proxyPost(t, p, proxyTestRequest(messages))
	if third.Code != http.StatusOK || !strings.Contains(third.Body.String(), `"content":"done"`) || !strings.Contains(third.Body.String(), "data: [DONE]") {
		t.Fatalf("final response: status %d, body %s", third.Code, third.Body.String())
	}

	replay := proxyPost(t, p, proxyTestRequest(messages))
	if replay.Code != http.StatusConflict || !strings.Contains(replay.Body.String(), "missing response state") {
		t.Fatalf("replay: status %d, body %s", replay.Code, replay.Body.String())
	}
	if round != 3 {
		t.Fatalf("upstream requests = %d, want 3", round)
	}
}

func TestProxyRejectsInvalidRequests(t *testing.T) {
	p := newProxy(proxyTestToken("test-token"), io.Discard)
	tests := []struct {
		name string
		body string
		code int
	}{
		{"unknown field", `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true,"surprise":1}`, http.StatusBadRequest},
		{"not streaming", `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":false}`, http.StatusBadRequest},
		{"unsupported role", `{"model":"m","messages":[{"role":"system","content":"x"}],"stream":true}`, http.StatusBadRequest},
		{"tools without parallel", `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true,"tools":[{"type":"function","function":{"name":"shell","description":"","strict":true,"parameters":{}}]}`, http.StatusBadRequest},
		{"missing cached state", `{"model":"m","messages":[{"role":"user","content":"x"},{"role":"assistant","content":"","tool_calls":[{"id":"missing","type":"function","function":{"name":"shell","arguments":"{}"}}]},{"role":"tool","content":"result","tool_call_id":"missing"}],"stream":true}`, http.StatusConflict},
		{"simple browser content type", `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`, http.StatusUnsupportedMediaType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response *httptest.ResponseRecorder
			if test.name == "simple browser content type" {
				response = proxyPostRawContentType(p, test.body, "text/plain")
			} else {
				response = proxyPostRaw(p, test.body)
			}
			if response.Code != test.code {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.code, response.Body.String())
			}
		})
	}
}

func TestProxyUpstreamFailures(t *testing.T) {
	t.Run("http usage limit", func(t *testing.T) {
		var stderr bytes.Buffer
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Request-Id", "request-1")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"limit reached"}}`)
		}))
		t.Cleanup(server.Close)
		p := newProxy(proxyTestToken("test-token"), &stderr)
		p.upstream = server.URL
		p.client = server.Client()
		response := proxyPost(t, p, proxyNoToolsRequest())
		if response.Code != http.StatusTooManyRequests || response.Header().Get("X-Request-Id") != "request-1" || response.Body.String() != `{"error":{"message":"limit reached"}}` {
			t.Fatalf("response: status %d, headers %v, body %s", response.Code, response.Header(), response.Body.String())
		}
		if !strings.Contains(stderr.String(), "a-chatgpt-proxy: POST /v1/chat/completions (upstream request request-1): upstream returned 429 Too Many Requests") {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})

	tests := []struct {
		name string
		body string
		want string
		log  string
	}{
		{"error event", "data: {\"type\":\"error\",\"code\":\"bad_request\",\"message\":\"bad\"}\n\n", `"code":"bad_request"`, "upstream error (bad_request): bad"},
		{"failed response", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"failed\",\"message\":\"bad\"}}}\n\n", `"code":"failed"`, "upstream response failed (failed): bad"},
		{"usage limit", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"subscription_sharing_usage_limit_exceeded\",\"message\":\"limit reached\"}}}\n\n", manageUsageURL, "upstream response failed (subscription_sharing_usage_limit_exceeded): limit reached Manage usage at " + manageUsageURL},
		{"incomplete response", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", "max_output_tokens", "upstream response incomplete: max_output_tokens"},
		{"malformed event", "data: not-json\n\n", "upstream_stream_error", "decode upstream event"},
		{"premature eof", "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"output_index\":0,\"content_index\":0,\"delta\":\"part\"}\n\n", "upstream_stream_ended", "upstream stream ended before response.completed"},
		{"keepalive then eof", "data: {\"type\":\"keepalive\"}\n\n", "upstream_stream_ended", "upstream stream ended before response.completed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			p := newProxy(proxyTestToken("test-token"), &stderr)
			p.upstream = server.URL
			p.client = server.Client()
			response := proxyPost(t, p, proxyNoToolsRequest())
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.want) || strings.Contains(response.Body.String(), "[DONE]") {
				t.Fatalf("response: status %d, body %s", response.Code, response.Body.String())
			}
			if !strings.Contains(stderr.String(), "a-chatgpt-proxy: POST /v1/chat/completions: "+test.log) {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestProxyReportsDownstreamStreamFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyResponseEvents(w, `{"type":"response.output_text.delta","item_id":"m","output_index":0,"content_index":0,"delta":"part"}`)
	}))
	t.Cleanup(server.Close)

	tests := []struct {
		name  string
		write error
		flush error
		want  string
	}{
		{"write", errors.New("broken pipe"), nil, "write downstream stream: broken pipe"},
		{"flush", nil, errors.New("broken pipe"), "flush downstream stream: broken pipe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			p := newProxy(proxyTestToken("test-token"), &stderr)
			p.upstream = server.URL
			p.client = server.Client()

			body, err := json.Marshal(proxyNoToolsRequest())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := &proxyFailingResponseWriter{writeErr: test.write, flushErr: test.flush}
			p.ServeHTTP(response, request)

			if response.writes != 1 {
				t.Fatalf("writes = %d, want 1", response.writes)
			}
			if !strings.Contains(stderr.String(), "a-chatgpt-proxy: POST /v1/chat/completions: "+test.want) {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}

	t.Run("terminal upstream error", func(t *testing.T) {
		terminalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			proxyResponseEvents(w, `{"type":"error","code":"bad_request","message":"bad"}`)
		}))
		t.Cleanup(terminalServer.Close)
		var stderr bytes.Buffer
		p := newProxy(proxyTestToken("test-token"), &stderr)
		p.upstream = terminalServer.URL
		p.client = terminalServer.Client()

		body, err := json.Marshal(proxyNoToolsRequest())
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := &proxyFailingResponseWriter{writeErr: errors.New("broken pipe")}
		p.ServeHTTP(response, request)

		if response.writes != 1 {
			t.Fatalf("writes = %d, want 1", response.writes)
		}
		want := "a-chatgpt-proxy: POST /v1/chat/completions: upstream error (bad_request): bad; write downstream error event: write downstream stream: broken pipe"
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})
}

func proxyTestRequest(messages []chatMessage) chatRequest {
	strict, stream, parallel := true, true, true
	return chatRequest{
		Messages: messages,
		Model:    "gpt-test",
		Stream:   &stream,
		Tools: []chatTool{{
			Type: "function",
			Function: chatFunctionDefinition{
				Name:        "shell",
				Description: "Run a command.",
				Strict:      &strict,
				Parameters:  json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}`),
			},
		}},
		ParallelToolCalls: &parallel,
	}
}

func proxyNoToolsRequest() chatRequest {
	stream := true
	return chatRequest{Messages: []chatMessage{proxyMessage("user", "x")}, Model: "gpt-test", Stream: &stream}
}

func proxyMessage(role, content string) chatMessage {
	return chatMessage{Role: role, Content: proxyString(content)}
}

func proxyString(value string) *string { return &value }

func proxyToolCall(id, command string) chatToolCall {
	arguments, _ := json.Marshal(struct {
		Command string `json:"command"`
	}{command})
	return chatToolCall{ID: id, Type: "function", Function: chatToolFunction{Name: "shell", Arguments: string(arguments)}}
}

func proxyDeltaCall(index int, call chatToolCall) chatDeltaToolCall {
	return chatDeltaToolCall{
		Index: index,
		ID:    call.ID,
		Type:  call.Type,
		Function: chatDeltaToolFunction{
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		},
	}
}

func proxyPost(t *testing.T, p *proxy, request chatRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return proxyPostRaw(p, string(body))
}

func proxyPostRaw(p *proxy, body string) *httptest.ResponseRecorder {
	return proxyPostRawContentType(p, body, "application/json")
}

func proxyPostRawContentType(p *proxy, body, contentType string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	p.ServeHTTP(response, request)
	return response
}

func proxyResponseEvents(w io.Writer, events ...string) {
	for _, event := range events {
		io.WriteString(w, "data: "+event+"\n\n")
	}
}

func proxyDoneEvent(index int, item string) string {
	return fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":%s}`, index, item)
}

func proxySSEData(t *testing.T, body string) []string {
	t.Helper()
	var data []string
	_, err := readSSE(strings.NewReader(body), func(event []byte) (bool, error) {
		data = append(data, string(event))
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func proxyAssertToolStream(t *testing.T, response *httptest.ResponseRecorder, want []chatDeltaToolCall) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var got []chatDeltaToolCall
	data := proxySSEData(t, response.Body.String())
	for _, event := range data {
		if event == "[DONE]" {
			continue
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(event), &chunk); err != nil {
			t.Fatal(err)
		}
		for _, choice := range chunk.Choices {
			got = append(got, choice.Delta.ToolCalls...)
		}
	}
	if !reflect.DeepEqual(got, want) || !strings.Contains(response.Body.String(), `"finish_reason":"tool_calls"`) || len(data) == 0 || data[len(data)-1] != "[DONE]" {
		t.Fatalf("tool stream = %#v, want %#v; body = %s", got, want, response.Body.String())
	}
}

func assertProxyItems(t *testing.T, got []json.RawMessage, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("input item count = %d, want %d; input = %s", len(got), len(want), got)
	}
	for i := range want {
		assertProxyJSON(t, got[i], want[i])
	}
}

func assertProxyJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode got JSON %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode want JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON = %s\nwant = %s", got, want)
	}
}
