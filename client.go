package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type completion struct {
	Message      message
	FinishReason string
}

type requestTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Strict      bool            `json:"strict"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func listModels(server string, out io.Writer) error {
	response, err := http.Get(strings.TrimRight(server, "/") + "/v1/models")
	if err != nil {
		return fmt.Errorf("request models: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("server returned %s: %q", response.Status, detail)
	}

	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
		return fmt.Errorf("decode models: %w", err)
	}
	for _, model := range catalog.Data {
		if _, err := fmt.Fprintln(out, model.ID); err != nil {
			return fmt.Errorf("write model: %w", err)
		}
	}
	return nil
}

func encodeRequest(model, effort string, messages []message, definitions []toolDefinition) ([]byte, error) {
	tools := make([]requestTool, len(definitions))
	for i, definition := range definitions {
		tools[i].Type = "function"
		tools[i].Function.Name = definition.name
		tools[i].Function.Description = definition.description
		tools[i].Function.Strict = true
		tools[i].Function.Parameters = definition.parameters
	}
	body, err := json.Marshal(struct {
		Messages          []message     `json:"messages"`
		Model             string        `json:"model"`
		Stream            bool          `json:"stream"`
		ReasoningEffort   string        `json:"reasoning_effort,omitempty"`
		Tools             []requestTool `json:"tools,omitempty"`
		ParallelToolCalls bool          `json:"parallel_tool_calls,omitempty"`
	}{
		Messages:          messages,
		Model:             model,
		Stream:            true,
		ReasoningEffort:   effort,
		Tools:             tools,
		ParallelToolCalls: len(tools) != 0,
	})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return body, nil
}

func streamCompletion(server, model, effort string, messages []message, definitions []toolDefinition, out, reasoningOut, status io.Writer) (result completion, err error) {
	body, err := encodeRequest(model, effort, messages, definitions)
	if err != nil {
		return result, err
	}
	if status == nil {
		status = io.Discard
	}
	endpoint := strings.TrimRight(server, "/") + "/v1/chat/completions"
	delay := time.Second
	var resp *http.Response
	for {
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return result, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")

		resp, err = http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			break
		}
		if err == nil && resp.StatusCode != http.StatusTooManyRequests && (resp.StatusCode < 500 || resp.StatusCode >= 600) {
			defer resp.Body.Close()
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return result, fmt.Errorf("server returned %s: %q", resp.Status, detail)
		}

		wait := delay
		failure := any(err)
		if err == nil {
			failure = resp.Status
			if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > wait {
				wait = retryAfter
			}
			resp.Body.Close()
		}
		if _, err := fmt.Fprintf(status, "… Completion request failed; retrying in %s\n  %v\n", wait, failure); err != nil {
			return result, fmt.Errorf("write retry status: %w", err)
		}
		time.Sleep(wait)
		delay *= 2
		if delay > time.Minute {
			delay = time.Minute
		}
	}
	defer resp.Body.Close()

	stream := completionStream{out: out, reasoningOut: reasoningOut}
	defer func() {
		if closeErr := stream.closeReasoning(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	reader := bufio.NewReader(resp.Body)
	var dataLines []string
	for {
		line, readErr := reader.ReadString('\n')
		if strings.HasSuffix(line, "\n") {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
		}
		if !utf8.ValidString(line) {
			return result, errors.New("server stream is not valid UTF-8")
		}

		if field, value, _ := strings.Cut(line, ":"); field == "data" {
			dataLines = append(dataLines, strings.TrimPrefix(value, " "))
		}

		eof := errors.Is(readErr, io.EOF)
		if line == "" || eof {
			done, err := consumeEvent(dataLines, &stream)
			if err != nil {
				return result, err
			}
			dataLines = nil
			if done {
				return completion{
					Message: message{
						Role:      "assistant",
						Content:   stream.answer.String(),
						Reasoning: stream.reasoning.String(),
						ToolCalls: stream.toolCalls(),
					},
					FinishReason: stream.finishReason,
				}, nil
			}
		}

		if eof {
			return result, errors.New("server stream ended before [DONE]")
		}
		if readErr != nil {
			return result, fmt.Errorf("read server stream: %w", readErr)
		}
	}
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return time.Until(date)
	}
	return 0
}

type completionStream struct {
	out           io.Writer
	reasoningOut  io.Writer
	answer        strings.Builder
	reasoning     strings.Builder
	reasoningOpen bool
	calls         []streamedToolCall
	finishReason  string
}

type streamedToolCall struct {
	seen bool
	call toolCall
}

func (s *completionStream) writeReasoning(content string) error {
	if content == "" {
		return nil
	}
	display := content
	if !s.reasoningOpen {
		display = "~ " + display
	}
	s.reasoningOpen = true
	if _, err := io.WriteString(s.reasoningOut, display); err != nil {
		return fmt.Errorf("write reasoning: %w", err)
	}
	s.reasoning.WriteString(content)
	return nil
}

func (s *completionStream) closeReasoning() error {
	if !s.reasoningOpen {
		return nil
	}
	s.reasoningOpen = false
	closing := "\n\n"
	if strings.HasSuffix(s.reasoning.String(), "\n") {
		closing = "\n"
	}
	if _, err := io.WriteString(s.reasoningOut, closing); err != nil {
		return fmt.Errorf("write reasoning: %w", err)
	}
	return nil
}

func (s *completionStream) writeContent(content string) error {
	if content == "" {
		return nil
	}
	if err := s.closeReasoning(); err != nil {
		return err
	}
	if _, err := io.WriteString(s.out, content); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	s.answer.WriteString(content)
	return nil
}

func (s *completionStream) writeToolCall(index int, fragment toolCall) error {
	if index < 0 {
		return fmt.Errorf("negative tool call index %d", index)
	}
	for len(s.calls) <= index {
		s.calls = append(s.calls, streamedToolCall{})
	}
	current := &s.calls[index]
	current.seen = true
	if fragment.ID != "" {
		if current.call.ID != "" && current.call.ID != fragment.ID {
			return fmt.Errorf("tool call %d changed id", index)
		}
		current.call.ID = fragment.ID
	}
	if fragment.Type != "" {
		if current.call.Type != "" && current.call.Type != fragment.Type {
			return fmt.Errorf("tool call %d changed type", index)
		}
		current.call.Type = fragment.Type
	}
	if fragment.Function.Name != "" {
		if current.call.Function.Name != "" && current.call.Function.Name != fragment.Function.Name {
			return fmt.Errorf("tool call %d changed name", index)
		}
		current.call.Function.Name = fragment.Function.Name
	}
	current.call.Function.Arguments += fragment.Function.Arguments
	return nil
}

func (s *completionStream) toolCalls() []toolCall {
	var calls []toolCall
	for _, streamed := range s.calls {
		if streamed.seen {
			calls = append(calls, streamed.call)
		}
	}
	return calls
}

func consumeEvent(dataLines []string, stream *completionStream) (bool, error) {
	if len(dataLines) == 0 {
		return false, nil
	}
	data := strings.Join(dataLines, "\n")
	if data == "[DONE]" {
		return true, nil
	}

	var chunk struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
				ReasoningText    string `json:"reasoning_text"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false, fmt.Errorf("decode server event: %w", err)
	}
	if len(chunk.Error) != 0 && string(chunk.Error) != "null" {
		return false, fmt.Errorf("server error: %s", chunk.Error)
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			return false, fmt.Errorf("unexpected choice index %d", choice.Index)
		}
		reasoning := choice.Delta.ReasoningContent
		if reasoning == "" {
			reasoning = choice.Delta.Reasoning
		}
		if reasoning == "" {
			reasoning = choice.Delta.ReasoningText
		}
		if err := stream.writeReasoning(reasoning); err != nil {
			return false, err
		}
		if err := stream.writeContent(choice.Delta.Content); err != nil {
			return false, err
		}
		for _, fragment := range choice.Delta.ToolCalls {
			call := toolCall{ID: fragment.ID, Type: fragment.Type}
			call.Function.Name = fragment.Function.Name
			call.Function.Arguments = fragment.Function.Arguments
			if err := stream.writeToolCall(fragment.Index, call); err != nil {
				return false, err
			}
		}
		if choice.FinishReason != "" {
			stream.finishReason = choice.FinishReason
		}
	}
	return false, nil
}
