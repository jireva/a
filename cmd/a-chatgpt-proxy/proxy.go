package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	defaultModelsEndpoint    = "https://api.openai.com/v1/models"
	defaultResponsesEndpoint = "https://api.openai.com/v1/responses"
)

type tokenSource interface {
	token(context.Context) (string, error)
}

type proxy struct {
	modelsURL string
	upstream  string
	client    *http.Client
	tokens    tokenSource
	errorLog  *log.Logger

	mu      sync.Mutex
	pending map[string]*responseState
}

type responseState struct {
	model string
	items []json.RawMessage
	calls []cachedCall
}

type cachedCall struct {
	id        string
	name      string
	arguments string
}

func newProxy(tokens tokenSource, stderr io.Writer) *proxy {
	return &proxy{
		modelsURL: defaultModelsEndpoint,
		upstream:  defaultResponsesEndpoint,
		client:    http.DefaultClient,
		tokens:    tokens,
		errorLog:  log.New(stderr, "a-chatgpt-proxy: ", 0),
	}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" && r.URL.RawQuery == "" {
		p.serveModels(w, r)
		return
	}
	if r.URL.Path != "/v1/chat/completions" || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	request, err := decodeChatRequest(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body, used, err := p.responsesRequest(request)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errMissingState) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	token, err := p.tokens.token(r.Context())
	if err != nil {
		p.httpError(w, r, http.StatusUnauthorized, fmt.Errorf("get access token: %w", err))
		return
	}

	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.upstream, bytes.NewReader(body))
	if err != nil {
		p.httpError(w, r, http.StatusInternalServerError, fmt.Errorf("create upstream request: %w", err))
		return
	}
	upstream.Header.Set("Authorization", "Bearer "+token)
	upstream.Header.Set("Content-Type", "application/json")
	upstream.Header.Set("Accept", "text/event-stream")

	response, err := p.client.Do(upstream)
	if err != nil {
		p.httpError(w, r, http.StatusBadGateway, fmt.Errorf("request upstream response: %w", err))
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		p.logRequestError(r, response.Header.Get("X-Request-Id"), fmt.Errorf("upstream returned %s", response.Status))
		copyHeader(w.Header(), response.Header, "Content-Type")
		copyHeader(w.Header(), response.Header, "X-Request-Id")
		w.WriteHeader(response.StatusCode)
		if _, err := io.Copy(w, response.Body); err != nil {
			p.logRequestError(r, response.Header.Get("X-Request-Id"), fmt.Errorf("forward upstream error response: %w", err))
		}
		return
	}

	copyHeader(w.Header(), response.Header, "X-Request-Id")
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := p.relay(w, response.Body, request.Model, used); err != nil {
		p.logRequestError(r, response.Header.Get("X-Request-Id"), err)
	}
}

func (p *proxy) serveModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, err := p.tokens.token(r.Context())
	if err != nil {
		p.httpError(w, r, http.StatusUnauthorized, fmt.Errorf("get access token: %w", err))
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.modelsURL, nil)
	if err != nil {
		p.httpError(w, r, http.StatusInternalServerError, fmt.Errorf("create upstream request: %w", err))
		return
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := p.client.Do(request)
	if err != nil {
		p.httpError(w, r, http.StatusBadGateway, fmt.Errorf("request upstream models: %w", err))
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		p.logRequestError(r, response.Header.Get("X-Request-Id"), fmt.Errorf("upstream returned %s", response.Status))
		copyHeader(w.Header(), response.Header, "Content-Type")
		copyHeader(w.Header(), response.Header, "X-Request-Id")
		w.WriteHeader(response.StatusCode)
		if _, err := io.Copy(w, response.Body); err != nil {
			p.logRequestError(r, response.Header.Get("X-Request-Id"), fmt.Errorf("forward upstream error response: %w", err))
		}
		return
	}

	var upstream struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.NewDecoder(response.Body).Decode(&upstream); err != nil {
		p.httpError(w, r, http.StatusBadGateway, fmt.Errorf("decode upstream models: %w", err))
		return
	}
	type listedModel struct {
		ID string `json:"id"`
	}
	models := make([]listedModel, 0, len(upstream.Models))
	for _, model := range upstream.Models {
		if model.Visibility == "list" {
			models = append(models, listedModel{ID: model.Slug})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Data []listedModel `json:"data"`
	}{models}); err != nil {
		p.logRequestError(r, response.Header.Get("X-Request-Id"), fmt.Errorf("write models response: %w", err))
	}
}

func (p *proxy) httpError(w http.ResponseWriter, r *http.Request, status int, err error) {
	p.logRequestError(r, "", err)
	http.Error(w, err.Error(), status)
}

func (p *proxy) logRequestError(r *http.Request, requestID string, err error) {
	if requestID == "" {
		p.errorLog.Printf("%s %s: %v", r.Method, r.URL.Path, err)
		return
	}
	p.errorLog.Printf("%s %s (upstream request %s): %v", r.Method, r.URL.Path, requestID, err)
}

func copyHeader(dst, src http.Header, name string) {
	if value := src.Get(name); value != "" {
		dst.Set(name, value)
	}
}

type chatRequest struct {
	Messages          []chatMessage `json:"messages"`
	Model             string        `json:"model"`
	Stream            *bool         `json:"stream"`
	ReasoningEffort   string        `json:"reasoning_effort,omitempty"`
	Tools             []chatTool    `json:"tools,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          *string        `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string                 `json:"type"`
	Function chatFunctionDefinition `json:"function"`
}

type chatFunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Strict      *bool           `json:"strict"`
	Parameters  json.RawMessage `json:"parameters"`
}

func decodeChatRequest(body io.Reader) (chatRequest, error) {
	var request chatRequest
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("decode request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return request, errors.New("request contains more than one JSON value")
		}
		return request, fmt.Errorf("decode request: %w", err)
	}
	if err := validateChatRequest(request); err != nil {
		return request, err
	}
	return request, nil
}

func validateChatRequest(request chatRequest) error {
	if request.Model == "" {
		return errors.New("model is required")
	}
	if request.Stream == nil || !*request.Stream {
		return errors.New("stream must be true")
	}
	if len(request.Messages) == 0 {
		return errors.New("messages must not be empty")
	}
	toolNames := make(map[string]bool, len(request.Tools))
	for i, tool := range request.Tools {
		if tool.Type != "function" {
			return fmt.Errorf("tool %d has unsupported type %q", i, tool.Type)
		}
		if tool.Function.Name == "" {
			return fmt.Errorf("tool %d has no name", i)
		}
		if toolNames[tool.Function.Name] {
			return fmt.Errorf("tool name %q is duplicated", tool.Function.Name)
		}
		toolNames[tool.Function.Name] = true
		if tool.Function.Strict == nil {
			return fmt.Errorf("tool %q omits strict", tool.Function.Name)
		}
		if len(tool.Function.Parameters) == 0 || !json.Valid(tool.Function.Parameters) {
			return fmt.Errorf("tool %q has invalid parameters", tool.Function.Name)
		}
	}
	if len(request.Tools) == 0 {
		if request.ParallelToolCalls != nil {
			return errors.New("parallel_tool_calls requires tools")
		}
	} else if request.ParallelToolCalls == nil || !*request.ParallelToolCalls {
		return errors.New("parallel_tool_calls must be true when tools are present")
	}

	pending := make(map[string]bool)
	seenCalls := make(map[string]bool)
	haveUser := false
	for i, message := range request.Messages {
		if message.Content == nil {
			return fmt.Errorf("message %d omits content", i)
		}
		switch message.Role {
		case "user":
			if message.ReasoningContent != "" || len(message.ToolCalls) != 0 || message.ToolCallID != "" {
				return fmt.Errorf("message %d is an invalid user message", i)
			}
			if len(pending) != 0 {
				return fmt.Errorf("message %d starts before all tool results", i)
			}
			haveUser = true

		case "assistant":
			if !haveUser || message.ToolCallID != "" || len(pending) != 0 {
				return fmt.Errorf("message %d is an invalid assistant message", i)
			}
			for j, call := range message.ToolCalls {
				if call.ID == "" || call.Type != "function" || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
					return fmt.Errorf("message %d has invalid tool call %d", i, j)
				}
				if seenCalls[call.ID] {
					return fmt.Errorf("tool call id %q is duplicated", call.ID)
				}
				seenCalls[call.ID] = true
				pending[call.ID] = true
			}

		case "tool":
			if !haveUser || message.ReasoningContent != "" || len(message.ToolCalls) != 0 || message.ToolCallID == "" || !pending[message.ToolCallID] {
				return fmt.Errorf("message %d is an invalid tool result", i)
			}
			delete(pending, message.ToolCallID)

		default:
			return fmt.Errorf("message %d has unsupported role %q", i, message.Role)
		}
	}
	if len(pending) != 0 {
		return errors.New("request omits one or more tool results")
	}
	lastRole := request.Messages[len(request.Messages)-1].Role
	if lastRole != "user" && lastRole != "tool" {
		return fmt.Errorf("request ends with role %q", lastRole)
	}
	return nil
}

type responsesRequest struct {
	Model             string               `json:"model"`
	Input             []json.RawMessage    `json:"input"`
	Store             bool                 `json:"store"`
	Stream            bool                 `json:"stream"`
	Reasoning         responsesReasoning   `json:"reasoning"`
	Tools             []responsesNamespace `json:"tools,omitempty"`
	ParallelToolCalls *bool                `json:"parallel_tool_calls,omitempty"`
}

type responsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary"`
}

type responsesNamespace struct {
	Type        string                  `json:"type"`
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	Tools       []responsesFunctionTool `json:"tools"`
}

type responsesFunctionTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Strict      bool            `json:"strict"`
	Parameters  json.RawMessage `json:"parameters"`
}

var errMissingState = errors.New("missing response state")

func (p *proxy) responsesRequest(request chatRequest) ([]byte, []*responseState, error) {
	lastUser := -1
	for i := range request.Messages {
		if request.Messages[i].Role == "user" {
			lastUser = i
		}
	}
	continuation := request.Messages[len(request.Messages)-1].Role == "tool"
	usedSet := make(map[*responseState]bool)
	var used []*responseState
	input := make([]json.RawMessage, 0, len(request.Messages))

	p.mu.Lock()
	defer p.mu.Unlock()
	for i, message := range request.Messages {
		switch message.Role {
		case "user":
			item, err := marshalRaw(struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}{message.Role, *message.Content})
			if err != nil {
				return nil, nil, err
			}
			input = append(input, item)

		case "assistant":
			active := continuation && i > lastUser && len(message.ToolCalls) != 0
			if active {
				state, err := p.lookupStateLocked(request.Model, message.ToolCalls)
				if err != nil {
					return nil, nil, err
				}
				for _, item := range state.items {
					input = append(input, append(json.RawMessage(nil), item...))
				}
				if !usedSet[state] {
					usedSet[state] = true
					used = append(used, state)
				}
				continue
			}
			if *message.Content != "" || len(message.ToolCalls) == 0 {
				item, err := marshalRaw(struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				}{message.Role, *message.Content})
				if err != nil {
					return nil, nil, err
				}
				input = append(input, item)
			}
			for _, call := range message.ToolCalls {
				item, err := marshalRaw(struct {
					Type      string `json:"type"`
					CallID    string `json:"call_id"`
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{"function_call", call.ID, "a", call.Function.Name, call.Function.Arguments})
				if err != nil {
					return nil, nil, err
				}
				input = append(input, item)
			}

		case "tool":
			item, err := marshalRaw(struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
				Output string `json:"output"`
			}{"function_call_output", message.ToolCallID, *message.Content})
			if err != nil {
				return nil, nil, err
			}
			input = append(input, item)
		}
	}

	upstream := responsesRequest{
		Model:  request.Model,
		Input:  input,
		Store:  false,
		Stream: true,
		Reasoning: responsesReasoning{
			Effort:  request.ReasoningEffort,
			Summary: "auto",
		},
	}
	if len(request.Tools) != 0 {
		functions := make([]responsesFunctionTool, len(request.Tools))
		for i, tool := range request.Tools {
			functions[i] = responsesFunctionTool{
				Type:        "function",
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				Strict:      *tool.Function.Strict,
				Parameters:  tool.Function.Parameters,
			}
		}
		upstream.Tools = []responsesNamespace{{
			Type:        "namespace",
			Name:        "a",
			Description: "Tools available through a.",
			Tools:       functions,
		}}
		upstream.ParallelToolCalls = request.ParallelToolCalls
	}
	body, err := json.Marshal(upstream)
	if err != nil {
		return nil, nil, fmt.Errorf("encode upstream request: %w", err)
	}
	return body, used, nil
}

func marshalRaw(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode input item: %w", err)
	}
	return data, nil
}

func (p *proxy) lookupStateLocked(model string, calls []chatToolCall) (*responseState, error) {
	if len(calls) == 0 || p.pending == nil {
		return nil, fmt.Errorf("%w for tool continuation", errMissingState)
	}
	state := p.pending[calls[0].ID]
	if state == nil {
		return nil, fmt.Errorf("%w for tool call %q", errMissingState, calls[0].ID)
	}
	if state.model != model || len(state.calls) != len(calls) {
		return nil, fmt.Errorf("%w: tool continuation does not match cached response", errMissingState)
	}
	for i, call := range calls {
		cached := state.calls[i]
		if p.pending[call.ID] != state || call.ID != cached.id || call.Function.Name != cached.name || call.Function.Arguments != cached.arguments {
			return nil, fmt.Errorf("%w: tool continuation does not match cached response", errMissingState)
		}
	}
	return state, nil
}

func (p *proxy) commitStates(used []*responseState, next *responseState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = make(map[string]*responseState)
	}
	if next == nil {
		for _, state := range used {
			for _, call := range state.calls {
				if p.pending[call.id] == state {
					delete(p.pending, call.id)
				}
			}
		}
		return nil
	}
	for _, call := range next.calls {
		if prior := p.pending[call.id]; prior != nil && prior != next {
			return fmt.Errorf("upstream reused tool call id %q", call.id)
		}
	}
	for _, call := range next.calls {
		p.pending[call.id] = next
	}
	return nil
}

type responseEvent struct {
	Type         string          `json:"type"`
	ItemID       string          `json:"item_id"`
	OutputIndex  *int            `json:"output_index"`
	ContentIndex *int            `json:"content_index"`
	SummaryIndex *int            `json:"summary_index"`
	Delta        json.RawMessage `json:"delta"`
	Item         json.RawMessage `json:"item"`
	Code         json.RawMessage `json:"code"`
	Message      string          `json:"message"`
	Param        json.RawMessage `json:"param"`
	Response     struct {
		Status            string          `json:"status"`
		Error             json.RawMessage `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

type outputItem struct {
	ID        string  `json:"id"`
	Type      string  `json:"type"`
	Status    string  `json:"status"`
	CallID    string  `json:"call_id"`
	Namespace string  `json:"namespace"`
	Name      string  `json:"name"`
	Arguments *string `json:"arguments"`
}

type responseStream struct {
	proxy *proxy
	out   *chatStream
	model string
	used  []*responseState

	doneItems         map[int]json.RawMessage
	summaryRecord     reasoningSummaryRecord
	haveSummaryRecord bool
	failure           error
}

type reasoningSummaryRecord struct {
	itemID       string
	outputIndex  int
	summaryIndex int
}

func (p *proxy) relay(w http.ResponseWriter, body io.Reader, model string, used []*responseState) error {
	stream := responseStream{
		proxy:     p,
		out:       &chatStream{w: w},
		model:     model,
		used:      used,
		doneItems: make(map[int]json.RawMessage),
	}
	done, err := readSSE(body, stream.consume)
	if err != nil {
		if stream.failure != nil {
			return fmt.Errorf("%w; write downstream error event: %w", stream.failure, err)
		}
		var downstream *downstreamError
		if errors.As(err, &downstream) {
			return err
		}
		if writeErr := stream.out.writeError("upstream_stream_error", err.Error()); writeErr != nil {
			return fmt.Errorf("%w; write downstream error event: %w", err, writeErr)
		}
		return err
	}
	if stream.failure != nil {
		return stream.failure
	}
	if !done {
		err := errors.New("upstream stream ended before response.completed")
		if writeErr := stream.out.writeError("upstream_stream_ended", err.Error()); writeErr != nil {
			return fmt.Errorf("%w; write downstream error event: %w", err, writeErr)
		}
		return err
	}
	return nil
}

func (s *responseStream) consume(data []byte) (bool, error) {
	if bytes.Equal(data, []byte("[DONE]")) {
		return false, errors.New("upstream sent [DONE] before a terminal Responses event")
	}
	var event responseEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return false, fmt.Errorf("decode upstream event: %w", err)
	}
	switch event.Type {
	case "keepalive",
		"response.created", "response.queued", "response.in_progress",
		"response.metadata",
		"response.output_item.added",
		"response.content_part.added", "response.content_part.done",
		"response.output_text.done", "response.refusal.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.done", "response.reasoning_text.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done":
		return false, nil

	case "response.reasoning_summary_text.delta":
		if err := requireIndexedDelta(event, true); err != nil {
			return false, err
		}
		delta, err := rawString(event.Delta, "reasoning summary delta")
		if err != nil {
			return false, err
		}
		if delta == "" {
			return false, nil
		}
		record := reasoningSummaryRecord{event.ItemID, *event.OutputIndex, *event.SummaryIndex}
		if s.haveSummaryRecord && record != s.summaryRecord {
			delta = "\n  " + delta
		}
		if err := s.out.writeReasoning(delta); err != nil {
			return false, err
		}
		s.summaryRecord = record
		s.haveSummaryRecord = true
		return false, nil

	case "response.reasoning_text.delta":
		if err := requireContentDelta(event); err != nil {
			return false, err
		}
		delta, err := rawString(event.Delta, "reasoning delta")
		if err != nil {
			return false, err
		}
		return false, s.out.writeReasoning(delta)

	case "response.output_text.delta", "response.refusal.delta":
		if err := requireContentDelta(event); err != nil {
			return false, err
		}
		delta, err := rawString(event.Delta, "text delta")
		if err != nil {
			return false, err
		}
		return false, s.out.writeContent(delta)

	case "response.output_item.done":
		return false, s.finishItem(event)

	case "response.completed":
		if event.Response.Status != "" && event.Response.Status != "completed" {
			return false, fmt.Errorf("response.completed has status %q", event.Response.Status)
		}
		if err := s.complete(); err != nil {
			return false, err
		}
		return true, nil

	case "error":
		errorJSON, err := eventError(event)
		if err != nil {
			return false, err
		}
		errorJSON = withUsageLink(errorJSON)
		s.failure = describeUpstreamFailure("upstream error", errorJSON)
		return true, s.out.writeRawError(errorJSON)

	case "response.failed":
		if len(event.Response.Error) == 0 || bytes.Equal(event.Response.Error, []byte("null")) {
			s.failure = errors.New("upstream response failed")
			return true, s.out.writeError("response_failed", "upstream response failed")
		}
		errorJSON := withUsageLink(event.Response.Error)
		s.failure = describeUpstreamFailure("upstream response failed", errorJSON)
		return true, s.out.writeRawError(errorJSON)

	case "response.incomplete":
		reason := "unknown"
		if event.Response.IncompleteDetails != nil && event.Response.IncompleteDetails.Reason != "" {
			reason = event.Response.IncompleteDetails.Reason
		}
		s.failure = errors.New("upstream response incomplete: " + reason)
		return true, s.out.writeError("response_incomplete", "upstream response incomplete: "+reason)

	default:
		return false, fmt.Errorf("unsupported upstream event %q", event.Type)
	}
}

func describeUpstreamFailure(kind string, raw json.RawMessage) error {
	var value struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &value) == nil && value.Message != "" {
		if value.Code != "" {
			return fmt.Errorf("%s (%s): %s", kind, value.Code, value.Message)
		}
		return fmt.Errorf("%s: %s", kind, value.Message)
	}
	return fmt.Errorf("%s: %s", kind, raw)
}

func requireIndexedDelta(event responseEvent, summary bool) error {
	if event.ItemID == "" || event.OutputIndex == nil || *event.OutputIndex < 0 || summary && (event.SummaryIndex == nil || *event.SummaryIndex < 0) {
		return fmt.Errorf("%s omits required indices", event.Type)
	}
	return nil
}

func requireContentDelta(event responseEvent) error {
	if event.ItemID == "" || event.OutputIndex == nil || *event.OutputIndex < 0 || event.ContentIndex == nil || *event.ContentIndex < 0 {
		return fmt.Errorf("%s omits required indices", event.Type)
	}
	return nil
}

func rawString(raw json.RawMessage, name string) (string, error) {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return "", fmt.Errorf("upstream %s is not a string", name)
	}
	return value, nil
}

func decodeOutputItem(event responseEvent) (outputItem, int, error) {
	var item outputItem
	if event.OutputIndex == nil || *event.OutputIndex < 0 || len(event.Item) == 0 {
		return item, 0, fmt.Errorf("%s omits output item fields", event.Type)
	}
	if err := json.Unmarshal(event.Item, &item); err != nil {
		return item, 0, fmt.Errorf("decode output item: %w", err)
	}
	if item.ID == "" || item.Type == "" {
		return item, 0, fmt.Errorf("%s contains an invalid output item", event.Type)
	}
	return item, *event.OutputIndex, nil
}

func (s *responseStream) finishItem(event responseEvent) error {
	item, outputIndex, err := decodeOutputItem(event)
	if err != nil {
		return err
	}
	if s.doneItems[outputIndex] != nil {
		return fmt.Errorf("output item %d completed twice", outputIndex)
	}
	switch item.Type {
	case "reasoning", "message":
	case "function_call":
		if item.CallID == "" || item.Name == "" || item.Arguments == nil || !json.Valid([]byte(*item.Arguments)) || item.Namespace != "" && item.Namespace != "a" {
			return errors.New("upstream completed an invalid function call")
		}
	default:
		return fmt.Errorf("unsupported output item type %q", item.Type)
	}
	s.doneItems[outputIndex] = append(json.RawMessage(nil), event.Item...)
	return nil
}

func (s *responseStream) complete() error {
	indices := make([]int, 0, len(s.doneItems))
	for index := range s.doneItems {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	items := make([]json.RawMessage, 0, len(indices))
	var calls []cachedCall
	callIDs := make(map[string]bool)
	for _, index := range indices {
		raw := append(json.RawMessage(nil), s.doneItems[index]...)
		items = append(items, raw)
		var item outputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return fmt.Errorf("decode completed output item: %w", err)
		}
		if item.Type == "function_call" {
			if callIDs[item.CallID] {
				return fmt.Errorf("upstream duplicated tool call id %q", item.CallID)
			}
			callIDs[item.CallID] = true
			calls = append(calls, cachedCall{id: item.CallID, name: item.Name, arguments: *item.Arguments})
		}
	}
	var next *responseState
	if len(calls) != 0 {
		next = &responseState{model: s.model, items: items, calls: calls}
		if err := s.out.writeCalls(calls); err != nil {
			return err
		}
		if err := s.proxy.commitStates(s.used, next); err != nil {
			return err
		}
	}
	finish := "stop"
	if len(calls) != 0 {
		finish = "tool_calls"
	}
	if err := s.out.finish(finish); err != nil {
		return err
	}
	if err := s.out.done(); err != nil {
		return err
	}
	if next == nil {
		return s.proxy.commitStates(s.used, nil)
	}
	return nil
}

func withUsageLink(raw json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return raw
	}
	var code string
	if json.Unmarshal(fields["code"], &code) != nil || code != "subscription_sharing_usage_limit_exceeded" {
		return raw
	}
	var message string
	if json.Unmarshal(fields["message"], &message) != nil {
		message = "ChatGPT plan usage limit reached."
	}
	message += " Manage usage at " + manageUsageURL
	encoded, err := json.Marshal(message)
	if err != nil {
		return raw
	}
	fields["message"] = encoded
	encoded, err = json.Marshal(fields)
	if err != nil {
		return raw
	}
	return encoded
}

func eventError(event responseEvent) (json.RawMessage, error) {
	if event.Message == "" {
		return nil, errors.New("upstream error event has no message")
	}
	type rawError struct {
		Code    json.RawMessage `json:"code,omitempty"`
		Message string          `json:"message"`
		Param   json.RawMessage `json:"param,omitempty"`
	}
	value := rawError{Code: event.Code, Message: event.Message, Param: event.Param}
	if len(value.Code) == 0 {
		value.Code = json.RawMessage(`"upstream_error"`)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode upstream error: %w", err)
	}
	return encoded, nil
}

type chatStream struct {
	w http.ResponseWriter
}

type chatChunk struct {
	Choices []chatChoice `json:"choices"`
}

type chatChoice struct {
	Index        int       `json:"index"`
	Delta        chatDelta `json:"delta"`
	FinishReason string    `json:"finish_reason,omitempty"`
}

type chatDelta struct {
	Content          *string             `json:"content,omitempty"`
	ReasoningContent *string             `json:"reasoning_content,omitempty"`
	ToolCalls        []chatDeltaToolCall `json:"tool_calls,omitempty"`
}

type chatDeltaToolCall struct {
	Index    int                   `json:"index"`
	ID       string                `json:"id,omitempty"`
	Type     string                `json:"type,omitempty"`
	Function chatDeltaToolFunction `json:"function"`
}

type chatDeltaToolFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

func (s *chatStream) writeReasoning(delta string) error {
	if delta == "" {
		return nil
	}
	return s.writeJSON(chatChunk{Choices: []chatChoice{{Index: 0, Delta: chatDelta{ReasoningContent: &delta}}}})
}

func (s *chatStream) writeContent(delta string) error {
	if delta == "" {
		return nil
	}
	return s.writeJSON(chatChunk{Choices: []chatChoice{{Index: 0, Delta: chatDelta{Content: &delta}}}})
}

func (s *chatStream) writeCalls(calls []cachedCall) error {
	toolCalls := make([]chatDeltaToolCall, len(calls))
	for i, call := range calls {
		toolCalls[i] = chatDeltaToolCall{
			Index: i,
			ID:    call.id,
			Type:  "function",
			Function: chatDeltaToolFunction{
				Name:      call.name,
				Arguments: call.arguments,
			},
		}
	}
	return s.writeJSON(chatChunk{Choices: []chatChoice{{Index: 0, Delta: chatDelta{ToolCalls: toolCalls}}}})
}

func (s *chatStream) finish(reason string) error {
	return s.writeJSON(chatChunk{Choices: []chatChoice{{Index: 0, Delta: chatDelta{}, FinishReason: reason}}})
}

func (s *chatStream) done() error {
	return s.writeData([]byte("[DONE]"))
}

func (s *chatStream) writeError(code, message string) error {
	encoded, err := json.Marshal(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message})
	if err != nil {
		return err
	}
	return s.writeRawError(encoded)
}

func (s *chatStream) writeRawError(raw json.RawMessage) error {
	if len(raw) == 0 || !json.Valid(raw) {
		return errors.New("invalid upstream error object")
	}
	payload := make([]byte, 0, len(raw)+10)
	payload = append(payload, `{"error":`...)
	payload = append(payload, raw...)
	payload = append(payload, '}')
	return s.writeData(payload)
}

func (s *chatStream) writeJSON(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.writeData(data)
}

type downstreamError struct {
	err error
}

func (e *downstreamError) Error() string { return e.err.Error() }
func (e *downstreamError) Unwrap() error { return e.err }

func (s *chatStream) writeData(data []byte) error {
	if _, err := s.w.Write(append(append([]byte("data: "), data...), '\n', '\n')); err != nil {
		return &downstreamError{fmt.Errorf("write downstream stream: %w", err)}
	}
	if err := http.NewResponseController(s.w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return &downstreamError{fmt.Errorf("flush downstream stream: %w", err)}
	}
	return nil
}

func readSSE(reader io.Reader, consume func([]byte) (bool, error)) (bool, error) {
	in := bufio.NewReader(reader)
	var dataLines []string
	for {
		line, err := in.ReadString('\n')
		if strings.HasSuffix(line, "\n") {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
		}
		if !utf8.ValidString(line) {
			return false, errors.New("upstream stream is not valid UTF-8")
		}
		if field, value, found := strings.Cut(line, ":"); found && field == "data" {
			dataLines = append(dataLines, strings.TrimPrefix(value, " "))
		}
		eof := errors.Is(err, io.EOF)
		if line == "" || eof {
			if len(dataLines) != 0 {
				done, consumeErr := consume([]byte(strings.Join(dataLines, "\n")))
				if consumeErr != nil || done {
					return done, consumeErr
				}
				dataLines = nil
			}
		}
		if eof {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read upstream stream: %w", err)
		}
	}
}
