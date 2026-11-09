package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Anthropic clients use Messages while engines continue to use Chat Completions.
type anthropicRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	Messages  []struct {
		Role         string          `json:"role"`
		Content      json.RawMessage `json:"content"`
		OutputConfig json.RawMessage `json:"output_config"`
		ClearAt      string          `json:"clear_at"`
	} `json:"messages"`
	System      json.RawMessage `json:"system"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	TopK        *int            `json:"top_k"`
	Stop        []string        `json:"stop_sequences"`
	Tools       []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
		Type        string          `json:"type"`
	} `json:"tools"`
	ToolChoice *struct {
		Type            string `json:"type"`
		Name            string `json:"name"`
		DisableParallel bool   `json:"disable_parallel_tool_use"`
	} `json:"tool_choice"`
	Thinking     json.RawMessage `json:"thinking"`
	OutputConfig json.RawMessage `json:"output_config"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

func anthropicBlocks(raw json.RawMessage) ([]anthropicBlock, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []anthropicBlock{{Type: "text", Text: text}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("content must be a string or an array of content blocks")
	}
	return blocks, nil
}

func translateAnthropic(raw []byte) ([]byte, anthropicRequest, error) {
	var in anthropicRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, in, err
	}
	in.Model = strings.TrimSpace(in.Model)
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	allowed := map[string]bool{"model": true, "max_tokens": true, "messages": true, "system": true, "stream": true, "temperature": true, "top_p": true, "top_k": true, "stop_sequences": true, "tools": true, "tool_choice": true, "thinking": true, "metadata": true, "output_config": true}
	for key := range fields {
		if !allowed[key] {
			return nil, in, fmt.Errorf("unsupported Anthropic option %q", key)
		}
	}
	// count_tokens requests carry no max_tokens. The Messages path rejects a
	// missing one itself, so that this translation serves both endpoints.
	if in.Model == "" || len(in.Messages) == 0 {
		return nil, in, errors.New("model and messages are required")
	}
	if in.MaxTokens < 0 {
		return nil, in, errors.New("max_tokens must be a positive integer")
	}
	messages := []any{}
	systemText := []string{}
	if len(in.System) > 0 && string(in.System) != "null" {
		blocks, err := anthropicBlocks(in.System)
		if err != nil {
			return nil, in, err
		}
		text := []string{}
		for _, b := range blocks {
			if b.Type != "text" {
				return nil, in, errors.New("system content must contain text only")
			}
			text = append(text, b.Text)
		}
		if joined := strings.Join(text, "\n"); joined != "" {
			systemText = append(systemText, joined)
		}
	}
	// Per-message effort changes apply to this generation in history order.
	messageControls := map[string]any{}
	lastUser := -1
	for i, m := range in.Messages {
		if m.Role == "user" {
			lastUser = i
		}
	}
	for i, m := range in.Messages {
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" {
			return nil, in, fmt.Errorf("messages[%d].role: unsupported role %q (expected user, assistant or system)", i, m.Role)
		}
		if m.Role != "system" && (m.ClearAt != "" || len(m.OutputConfig) > 0 && string(m.OutputConfig) != "null") {
			return nil, in, fmt.Errorf("messages[%d]: clear_at and output_config require the system role", i)
		}
		if m.Role == "system" {
			if m.ClearAt != "" && m.ClearAt != "never" && m.ClearAt != "next_user_message" {
				return nil, in, fmt.Errorf("messages[%d].clear_at: unsupported value %q", i, m.ClearAt)
			}
			if m.ClearAt == "next_user_message" && len(m.OutputConfig) > 0 && string(m.OutputConfig) != "null" {
				return nil, in, fmt.Errorf("messages[%d]: output_config cannot be combined with clear_at next_user_message", i)
			}
			if err := translateSystemOutputConfig(m.OutputConfig, messageControls); err != nil {
				return nil, in, fmt.Errorf("messages[%d]: %w", i, err)
			}
			var blocks []anthropicBlock
			if len(m.Content) > 0 {
				var err error
				blocks, err = anthropicBlocks(m.Content)
				if err != nil {
					return nil, in, fmt.Errorf("messages[%d].content: %w", i, err)
				}
			}
			text := []string{}
			for _, b := range blocks {
				if b.Type != "text" {
					return nil, in, fmt.Errorf("messages[%d]: system content must contain text only (unsupported block %q)", i, b.Type)
				}
				text = append(text, b.Text)
			}
			// Expired turn-scoped reminders and effort-only messages render no text.
			if !(m.ClearAt == "next_user_message" && i < lastUser) && len(text) > 0 && strings.Join(text, "\n") != "" {
				systemText = append(systemText, strings.Join(text, "\n"))
			}
			continue
		}
		blocks, err := anthropicBlocks(m.Content)
		if err != nil {
			return nil, in, err
		}
		parts := []any{}
		reasoning := []string{}
		calls := []any{}
		results := []any{}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			case "thinking", "redacted_thinking":
				if m.Role != "assistant" {
					return nil, in, errors.New("thinking blocks require an assistant message")
				}
				// Claude signatures cannot be checked by a local engine. Preserve
				// readable reasoning separately; opaque redacted blocks are omitted.
				if b.Type == "thinking" {
					reasoning = append(reasoning, b.Thinking)
				}
			case "image":
				if m.Role != "user" || b.Source == nil {
					return nil, in, errors.New("images require a source and a user message")
				}
				var url string
				switch b.Source.Type {
				case "base64":
					url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
				case "url":
					url = b.Source.URL
				default:
					return nil, in, errors.New("unsupported image source")
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
			case "tool_use":
				if m.Role != "assistant" || b.ID == "" || b.Name == "" || !json.Valid(b.Input) {
					return nil, in, errors.New("invalid tool_use block")
				}
				calls = append(calls, map[string]any{"id": b.ID, "type": "function", "function": map[string]any{"name": b.Name, "arguments": string(b.Input)}})
			case "tool_result":
				if m.Role != "user" || b.ToolUseID == "" {
					return nil, in, errors.New("invalid tool_result block")
				}
				var text string
				if len(b.Content) > 0 {
					bs, err := anthropicBlocks(b.Content)
					if err != nil {
						return nil, in, err
					}
					ts := []string{}
					for _, v := range bs {
						if v.Type != "text" {
							return nil, in, errors.New("tool results currently support text only")
						}
						ts = append(ts, v.Text)
					}
					text = strings.Join(ts, "\n")
				}
				// A tool message has no error flag, so a failed call has to say
				// so in its text or the model reads it as a normal result.
				if b.IsError {
					if text == "" {
						text = "Error"
					} else {
						text = "Error: " + text
					}
				}
				results = append(results, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": text})
			default:
				return nil, in, fmt.Errorf("unsupported content block %q", b.Type)
			}
		}
		messages = append(messages, results...)
		if len(parts) > 0 || len(calls) > 0 || len(results) == 0 {
			var content any = parts
			if len(parts) == 0 {
				content = nil
			} else if len(parts) == 1 {
				if v, ok := parts[0].(map[string]any); ok && v["type"] == "text" {
					content = v["text"]
				}
			}
			msg := map[string]any{"role": m.Role, "content": content}
			if len(reasoning) > 0 {
				msg["reasoning_content"] = strings.Join(reasoning, "\n")
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			messages = append(messages, msg)
		}
	}
	// Many engine chat templates allow exactly one system message, first.
	// Combine active instructions in their original order without disturbing
	// user/assistant turns or separating tool calls from their results.
	if len(systemText) > 0 {
		messages = append([]any{map[string]any{"role": "system", "content": strings.Join(systemText, "\n")}}, messages...)
	}
	out := map[string]any{"model": in.Model, "messages": messages, "stream": in.Stream}
	if in.MaxTokens > 0 {
		out["max_tokens"] = in.MaxTokens
	}
	if err := translateOutputConfig(in.OutputConfig, out); err != nil {
		return nil, in, err
	}
	for key, value := range messageControls {
		out[key] = value
	}
	if err := translateThinking(in.Thinking, in.MaxTokens, out); err != nil {
		return nil, in, err
	}
	if in.Stream {
		out["stream_options"] = map[string]bool{"include_usage": true}
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if in.TopK != nil {
		out["top_k"] = *in.TopK
	}
	if len(in.Stop) > 0 {
		out["stop"] = in.Stop
	}
	tools := []any{}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "custom" {
			return nil, in, errors.New("server tools are not supported")
		}
		if t.Name == "" || !json.Valid(t.InputSchema) {
			return nil, in, errors.New("tools require name and input_schema")
		}
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}})
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if t := in.ToolChoice; t != nil {
		switch t.Type {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			if t.Name == "" {
				return nil, in, errors.New("tool_choice requires a name")
			}
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": t.Name}}
		default:
			return nil, in, errors.New("unsupported tool_choice")
		}
		out["parallel_tool_calls"] = !t.DisableParallel
	}
	body, err := json.Marshal(out)
	return body, in, err
}

// System messages can change effort, but not the response schema. Keep the
// regular output_config validation and mapping for the supported field.
func translateSystemOutputConfig(raw json.RawMessage, out map[string]any) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("invalid system output_config: %w", err)
	}
	for key := range fields {
		if key != "effort" {
			return fmt.Errorf("unsupported system output_config option %q", key)
		}
	}
	return translateOutputConfig(raw, out)
}

// Thinking controls use llama.cpp-compatible request fields. Other engines
// may ignore these extensions; no Anthropic thinking transcript is generated.
func translateThinking(raw json.RawMessage, maxTokens int, out map[string]any) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var thinking struct {
		Type    string `json:"type"`
		Budget  *int   `json:"budget_tokens"`
		Display string `json:"display"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&thinking); err != nil {
		return fmt.Errorf("invalid thinking: %w", err)
	}
	if thinking.Display != "" && thinking.Display != "summarized" && thinking.Display != "omitted" {
		return fmt.Errorf("unsupported thinking.display %q", thinking.Display)
	}
	switch thinking.Type {
	case "enabled":
		if thinking.Budget == nil || *thinking.Budget < 1024 || maxTokens > 0 && *thinking.Budget >= maxTokens {
			return errors.New("thinking.budget_tokens must be at least 1024 and less than max_tokens")
		}
		out["reasoning_budget_tokens"] = *thinking.Budget
	case "adaptive", "disabled":
		if thinking.Budget != nil {
			return errors.New("thinking.budget_tokens is only supported with type enabled")
		}
	default:
		return fmt.Errorf("unsupported thinking.type %q (expected enabled, adaptive or disabled)", thinking.Type)
	}
	enabled := thinking.Type != "disabled"
	out["chat_template_kwargs"] = map[string]bool{"enable_thinking": enabled}
	if !enabled {
		out["reasoning_effort"] = "none"
	}
	return nil
}

// Translate output controls rather than passing Anthropic-specific keys to
// engines. Preserve xhigh; max uses the highest common reasoning level.
func translateOutputConfig(raw json.RawMessage, out map[string]any) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var config struct {
		Effort string `json:"effort"`
		Format *struct {
			Type   string          `json:"type"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("invalid output_config: %w", err)
	}
	if config.Effort != "" {
		effort := config.Effort
		switch effort {
		case "low", "medium", "high", "xhigh":
		case "max":
			effort = "xhigh"
		default:
			return fmt.Errorf("unsupported output_config.effort %q", effort)
		}
		out["reasoning_effort"] = effort
	}
	if f := config.Format; f != nil {
		if f.Type != "json_schema" {
			return fmt.Errorf("unsupported output_config.format.type %q", f.Type)
		}
		var schema map[string]json.RawMessage
		if err := json.Unmarshal(f.Schema, &schema); err != nil || schema == nil {
			return errors.New("output_config.format.schema must be a JSON object")
		}
		out["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "strict": true, "schema": f.Schema}}
	}
	return nil
}

func writeAnthropicError(w http.ResponseWriter, status int, msg, kind string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": msg}})
}

func anthropicStop(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

type openAIReply struct {
	ID      string `json:"id"`
	Choices []struct {
		Message struct {
			Content   *string      `json:"content"`
			Reasoning string       `json:"reasoning_content"`
			ToolCalls []openAITool `json:"tool_calls"`
		} `json:"message"`
		Delta struct {
			Content   string       `json:"content"`
			Reasoning string       `json:"reasoning_content"`
			ToolCalls []openAITool `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		Input  int `json:"prompt_tokens"`
		Output int `json:"completion_tokens"`
	} `json:"usage"`
}
type openAITool struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

var effortRejection = regexp.MustCompile(`Unexpected reasoning effort ([a-z]+)\. Supported types are ([^\r\n.]+)`)
var effortNames = regexp.MustCompile(`\b(low|medium|high|xhigh|max)\b`)

// Only a specific template-validation failure is safe to retry: the prompt
// was rejected before generation. Never retry timeouts, arbitrary 500s or SSE.
func compatibleEffort(message, requested string) string {
	ranks := map[string]int{"low": 0, "medium": 1, "high": 2, "xhigh": 3, "max": 4}
	rank, ok := ranks[requested]
	if !ok {
		return ""
	} // In particular, never enable disabled reasoning.
	match := effortRejection.FindStringSubmatch(message)
	if len(match) != 3 || match[1] != requested {
		return ""
	}
	names := effortNames.FindAllString(match[2], -1)
	best := ""
	distance := 100
	for _, name := range names {
		if name == requested {
			return ""
		} // A contradictory error is not a capability list.
		d := ranks[name] - rank
		if d < 0 {
			d = -d
		}
		if d < distance || d == distance && ranks[name] > ranks[best] {
			best = name
			distance = d
		}
	}
	return best
}

func (rt *Router) retryAnthropicEffort(req *http.Request, resp *http.Response, target routeTarget) (*http.Response, error) {
	if (resp.StatusCode != 400 && resp.StatusCode != 500) || req.GetBody == nil || req.Context().Err() != nil {
		return resp, nil
	}
	const limit = 1 << 20
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	// Restore the inspected prefix, keeping the original closer and remainder.
	original := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(data), original), original}
	if readErr != nil || len(data) > limit {
		return resp, nil
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &payload) != nil || !effortRejection.MatchString(payload.Error.Message) {
		return resp, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return resp, nil
	}
	var fields map[string]json.RawMessage
	err = json.NewDecoder(body).Decode(&fields)
	body.Close()
	if err != nil {
		return resp, nil
	}
	var effort string
	if json.Unmarshal(fields["reasoning_effort"], &effort) != nil {
		return resp, nil
	}
	replacement := compatibleEffort(payload.Error.Message, effort)
	if replacement == "" {
		return resp, nil
	}
	fields["reasoning_effort"], _ = json.Marshal(replacement)
	encoded, err := json.Marshal(fields)
	if err != nil {
		return resp, nil
	}
	retry := req.Clone(req.Context())
	retry.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }
	retry.Body, _ = retry.GetBody()
	retry.ContentLength = int64(len(encoded))
	retry.TransferEncoding = nil
	retry.Header.Set("Content-Length", strconv.Itoa(len(encoded)))
	resp.Body.Close()
	if rt.throttle.allow("effort "+target.String()+" "+effort+" "+replacement, 10*time.Second) {
		rt.mgr.Log().System(fmt.Sprintf("engine %q rejected reasoning effort %s; retrying %s with supported effort %s", target.engine, effort, target.model, replacement))
	}
	return rt.proxy.Transport.RoundTrip(retry)
}

func (rt *Router) anthropicMessages(w http.ResponseWriter, r *http.Request, target routeTarget, in anthropicRequest) {
	req := r.Clone(r.Context())
	req.URL.Scheme = "http"
	req.URL.Host = fmt.Sprintf("%s:%d", engineHost, target.port)
	req.URL.Path = "/v1/chat/completions"
	req.URL.RawPath = ""
	req.URL.RawQuery = ""
	req.RequestURI = ""
	req.Header = r.Header.Clone()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Del("x-api-key")
	req.Header.Del("anthropic-version")
	req.Header.Del("anthropic-beta")
	resp, err := rt.proxy.Transport.RoundTrip(req)
	if err == nil {
		resp, err = rt.retryAnthropicEffort(req, resp, target)
	}
	if err != nil {
		if r.Context().Err() == nil {
			writeAnthropicError(w, 502, "the engine did not answer: "+err.Error(), "api_error")
		}
		return
	}
	defer resp.Body.Close()
	w.Header().Set(HeaderUpstream, target.String())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rt.logUpstreamError(target, r.Method, req.URL.Path, resp.StatusCode)
		msg := "engine returned " + resp.Status
		var payload struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload) == nil && payload.Error.Message != "" {
			msg = payload.Error.Message
		}
		kind := "api_error"
		if resp.StatusCode == 400 {
			kind = "invalid_request_error"
		}
		if resp.StatusCode == 404 {
			kind = "not_found_error"
		}
		if resp.StatusCode == 429 {
			kind = "rate_limit_error"
		}
		writeAnthropicError(w, resp.StatusCode, msg, kind)
		return
	}
	if in.Stream {
		streamAnthropic(w, resp.Body, in.Model)
		return
	}
	var reply openAIReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxInspectBody)).Decode(&reply); err != nil || len(reply.Choices) == 0 {
		writeAnthropicError(w, 502, "invalid Chat Completions response from engine", "api_error")
		return
	}
	c := reply.Choices[0]
	content := []any{}
	// Engines reason in plain text. A local signature would be meaningless, so
	// the block carries an empty one and translates back on the next request.
	if c.Message.Reasoning != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": c.Message.Reasoning, "signature": ""})
	}
	if c.Message.Content != nil && *c.Message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": *c.Message.Content})
	}
	for _, t := range c.Message.ToolCalls {
		var input map[string]any
		if json.Unmarshal([]byte(t.Function.Arguments), &input) != nil || input == nil {
			writeAnthropicError(w, 502, "engine returned invalid tool arguments", "api_error")
			return
		}
		content = append(content, map[string]any{"type": "tool_use", "id": t.ID, "name": t.Function.Name, "input": input})
	}
	writeJSON(w, 200, map[string]any{"id": reply.ID, "type": "message", "role": "assistant", "model": in.Model, "content": content, "stop_reason": anthropicStop(c.FinishReason), "stop_sequence": nil, "usage": map[string]int{"input_tokens": reply.Usage.Input, "output_tokens": reply.Usage.Output}})
}

// enginePost calls a native engine endpoint outside the OpenAI surface. A
// failure is an answer: the only caller has an approximation to fall back on.
func (rt *Router) enginePost(ctx context.Context, target routeTarget, path string, payload any) []byte {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	url := fmt.Sprintf("http://%s:%d%s", engineHost, target.port, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := rt.proxy.Transport.RoundTrip(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxInspectBody))
	if err != nil {
		return nil
	}
	return data
}

// Engines share no token-counting endpoint. llama.cpp can render the prompt
// and tokenize it exactly, which costs no generation; anything else gets a
// size estimate, because a rough count beats refusing to answer at all.
func (rt *Router) anthropicCountTokens(w http.ResponseWriter, r *http.Request, target routeTarget) {
	raw := []byte{}
	if r.GetBody != nil {
		if body, err := r.GetBody(); err == nil {
			raw, _ = io.ReadAll(body)
			body.Close()
		}
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	template := map[string]json.RawMessage{}
	for _, key := range []string{"messages", "tools", "chat_template_kwargs"} {
		if value, ok := fields[key]; ok {
			template[key] = value
		}
	}
	text := string(raw)
	if data := rt.enginePost(r.Context(), target, "/apply-template", template); data != nil {
		var rendered struct {
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal(data, &rendered) == nil && rendered.Prompt != "" {
			text = rendered.Prompt
			data = rt.enginePost(r.Context(), target, "/tokenize", map[string]string{"content": rendered.Prompt})
			var tokenized struct {
				Tokens []json.RawMessage `json:"tokens"`
			}
			if data != nil && json.Unmarshal(data, &tokenized) == nil && len(tokenized.Tokens) > 0 {
				w.Header().Set(HeaderUpstream, target.String())
				writeJSON(w, 200, map[string]int{"input_tokens": len(tokenized.Tokens)})
				return
			}
		}
	}
	if rt.throttle.allow("count_tokens "+target.String(), time.Minute) {
		rt.mgr.Log().System(fmt.Sprintf("engine %q cannot tokenize; token counts for %s are estimated", target.engine, target.model))
	}
	// Four characters per token is the usual rule of thumb for prose and code.
	writeJSON(w, 200, map[string]int{"input_tokens": (utf8.RuneCountInString(text) + 3) / 4})
}

// OpenAI chunks can interleave tool arguments. Each tool gets a stable block
// index, and all blocks close before the terminal message_delta.
func streamAnthropic(w http.ResponseWriter, body io.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	emit := func(kind string, v map[string]any) {
		v["type"] = kind
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	started := false
	invalid := false
	next := 0
	thinkIndex := -1
	textIndex := -1
	tools := map[int]int{}
	reason := ""
	input, output := 0, 0
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), int(maxInspectBody))
	process := func(data string) bool {
		if data == "[DONE]" {
			return false
		}
		var chunk openAIReply
		if json.Unmarshal([]byte(data), &chunk) != nil {
			invalid = true
			emit("error", map[string]any{"error": map[string]string{"type": "api_error", "message": "invalid SSE data from engine"}})
			return false
		}
		if !started {
			started = true
			emit("message_start", map[string]any{"message": map[string]any{"id": chunk.ID, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": chunk.Usage.Input, "output_tokens": 0}}})
		}
		if chunk.Usage.Input > 0 {
			input = chunk.Usage.Input
		}
		if chunk.Usage.Output > 0 {
			output = chunk.Usage.Output
		}
		if len(chunk.Choices) == 0 {
			return true
		}
		c := chunk.Choices[0]
		if c.FinishReason != "" {
			reason = c.FinishReason
		}
		// Reasoning arrives before content, so its block takes the lower index.
		if c.Delta.Reasoning != "" {
			if thinkIndex < 0 {
				thinkIndex = next
				next++
				emit("content_block_start", map[string]any{"index": thinkIndex, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}})
			}
			emit("content_block_delta", map[string]any{"index": thinkIndex, "delta": map[string]string{"type": "thinking_delta", "thinking": c.Delta.Reasoning}})
		}
		if c.Delta.Content != "" {
			if textIndex < 0 {
				textIndex = next
				next++
				emit("content_block_start", map[string]any{"index": textIndex, "content_block": map[string]any{"type": "text", "text": ""}})
			}
			emit("content_block_delta", map[string]any{"index": textIndex, "delta": map[string]string{"type": "text_delta", "text": c.Delta.Content}})
		}
		for _, t := range c.Delta.ToolCalls {
			idx, ok := tools[t.Index]
			if !ok {
				idx = next
				next++
				tools[t.Index] = idx
				emit("content_block_start", map[string]any{"index": idx, "content_block": map[string]any{"type": "tool_use", "id": t.ID, "name": t.Function.Name, "input": map[string]any{}}})
			}
			if t.Function.Arguments != "" {
				emit("content_block_delta", map[string]any{"index": idx, "delta": map[string]string{"type": "input_json_delta", "partial_json": t.Function.Arguments}})
			}
		}
		return true
	}
	data := []string{}
	failed := false
	done := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) > 0 {
				if !process(strings.Join(data, "\n")) {
					done = true
					break
				}
				data = nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if !done && len(data) > 0 {
		process(strings.Join(data, "\n"))
	}
	if invalid {
		return
	}
	if scanner.Err() != nil || !started || reason == "" {
		failed = true
	}
	if failed {
		emit("error", map[string]any{"error": map[string]string{"type": "api_error", "message": "engine stream ended before a complete response"}})
		return
	}
	for i := 0; i < next; i++ {
		emit("content_block_stop", map[string]any{"index": i})
	}
	emit("message_delta", map[string]any{"delta": map[string]any{"stop_reason": anthropicStop(reason), "stop_sequence": nil}, "usage": map[string]int{"input_tokens": input, "output_tokens": output}})
	emit("message_stop", map[string]any{})
}

// prepareAnthropic validates before starting an engine and restores a translated
// body for both model inspection and the upstream request.
func prepareAnthropic(r *http.Request) (anthropicRequest, error) {
	if r.Body == nil {
		return anthropicRequest{}, errors.New("request body is required")
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxInspectBody+1))
	if err != nil {
		return anthropicRequest{}, err
	}
	if len(raw) > maxInspectBody {
		return anthropicRequest{}, errors.New("Anthropic request exceeds 64 MiB")
	}
	body, in, err := translateAnthropic(raw)
	if err != nil {
		return in, err
	}
	if model := strings.TrimSpace(r.URL.Query().Get("model")); model != "" {
		in.Model = model
		var obj map[string]any
		json.Unmarshal(body, &obj)
		obj["model"] = model
		body, _ = json.Marshal(obj)
	}
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.Body, _ = r.GetBody()
	r.ContentLength = int64(len(body))
	r.TransferEncoding = nil
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return in, nil
}
