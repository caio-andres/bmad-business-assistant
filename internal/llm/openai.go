package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// OpenAIConfig configures an OpenAI-compatible endpoint.
type OpenAIConfig struct {
	BaseURL      string            // e.g. https://openrouter.ai/api/v1
	APIKey       string            // sent as Bearer; may be empty (Ollama, Kong with key-auth upstream)
	Headers      map[string]string // extra headers (gateway routing, OpenRouter attribution)
	Stream       bool              // use SSE streaming
	StreamUsage  bool              // send stream_options.include_usage
	MaxRetries   int               // transient retries before failing
	Timeout      time.Duration     // per request (whole stream)
	HTTPClient   *http.Client
	RetryBackoff time.Duration // base backoff (default 1s)
}

// OpenAI is a client for the Chat Completions API.
type OpenAI struct {
	cfg  OpenAIConfig
	http *http.Client
}

// NewOpenAI builds a client with sane defaults.
func NewOpenAI(cfg OpenAIConfig) *OpenAI {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryBackoff == 0 {
		cfg.RetryBackoff = time.Second
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &OpenAI{cfg: cfg, http: hc}
}

func (o *OpenAI) Name() string { return "openai" }

type wireToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func toWire(msgs []Message) []wireMessage {
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		wm := wireMessage{Role: m.Role, Content: &content, ToolCallID: m.ToolCallID}
		if m.Role == RoleTool {
			wm.Name = m.Name
		}
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			if content == "" {
				wm.Content = nil
			}
			for _, tc := range m.ToolCalls {
				w := wireToolCall{ID: tc.ID, Type: "function"}
				w.Function.Name = tc.Name
				w.Function.Arguments = tc.Arguments
				if w.Function.Arguments == "" {
					w.Function.Arguments = "{}"
				}
				wm.ToolCalls = append(wm.ToolCalls, w)
			}
		}
		out = append(out, wm)
	}
	return out
}

func (o *OpenAI) body(req Request, stream bool) ([]byte, error) {
	payload := map[string]any{
		"model":    req.Model,
		"messages": toWire(req.Messages),
	}
	if len(req.Tools) > 0 {
		tools := make([]wireTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			var wt wireTool
			wt.Type = "function"
			wt.Function.Name = t.Name
			wt.Function.Description = t.Description
			wt.Function.Parameters = t.Parameters
			tools = append(tools, wt)
		}
		payload["tools"] = tools
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.JSONMode {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	if stream {
		payload["stream"] = true
		if o.cfg.StreamUsage {
			payload["stream_options"] = map[string]bool{"include_usage": true}
		}
	}
	return json.Marshal(payload)
}

// StatusError is a non-2xx response from the provider.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	b := e.Body
	if len(b) > 300 {
		b = b[:300] + "..."
	}
	return fmt.Sprintf("llm: HTTP %d: %s", e.Code, b)
}

func transient(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == 429 || se.Code == 408 || se.Code >= 500
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	return true // network errors, timeouts
}

// Chat sends the request, retrying transient failures before any byte is streamed.
func (o *OpenAI) Chat(ctx context.Context, req Request, onText func(string)) (*Response, error) {
	var lastErr error
	for attempt := 0; attempt <= o.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := o.cfg.RetryBackoff * time.Duration(1<<(attempt-1))
			slog.Warn("llm retry", "attempt", attempt, "wait", wait, "err", lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
		streamed := false
		resp, err := o.once(ctx, req, func(d string) {
			streamed = true
			if onText != nil {
				onText(d)
			}
		})
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if streamed || !transient(err) || ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("%w: %v", ErrUnavailable, lastErr)
}

func (o *OpenAI) once(ctx context.Context, req Request, onText func(string)) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, o.cfg.Timeout)
	defer cancel()
	body, err := o.body(req, o.cfg.Stream)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}
	for k, v := range o.cfg.Headers {
		httpReq.Header.Set(k, v)
	}
	if o.cfg.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	res, err := o.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, &StatusError{Code: res.StatusCode, Body: string(b)}
	}
	if !o.cfg.Stream || !strings.Contains(res.Header.Get("Content-Type"), "event-stream") {
		return o.decodeFull(res.Body, req.Model, onText)
	}
	return o.decodeStream(res.Body, req.Model, onText)
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (o *OpenAI) decodeFull(r io.Reader, model string, onText func(string)) (*Response, error) {
	var payload struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      wireMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
		Usage *wireUsage `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 16<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("llm: decode: %w", err)
	}
	if len(payload.Choices) == 0 {
		return nil, errors.New("llm: resposta sem choices")
	}
	c := payload.Choices[0]
	msg := Message{Role: RoleAssistant}
	if c.Message.Content != nil {
		msg.Content = *c.Message.Content
	}
	for i, tc := range c.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: id, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	if onText != nil && msg.Content != "" {
		onText(msg.Content)
	}
	resp := &Response{Message: msg, FinishReason: c.FinishReason, Model: firstNonEmpty(payload.Model, model)}
	if payload.Usage != nil {
		resp.Usage = Usage{InputTokens: payload.Usage.PromptTokens, OutputTokens: payload.Usage.CompletionTokens}
	}
	return resp, nil
}

func (o *OpenAI) decodeStream(r io.Reader, model string, onText func(string)) (*Response, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var content strings.Builder
	type partial struct {
		id, name string
		args     strings.Builder
	}
	var calls []*partial
	resp := &Response{Model: model}
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content   *string        `json:"content"`
					ToolCalls []wireToolCall `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *wireUsage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("llm: erro no stream: %s", chunk.Error.Message)
		}
		if chunk.Model != "" {
			resp.Model = chunk.Model
		}
		if chunk.Usage != nil {
			resp.Usage = Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != nil && *ch.Delta.Content != "" {
				content.WriteString(*ch.Delta.Content)
				if onText != nil {
					onText(*ch.Delta.Content)
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				idx := len(calls)
				if tc.Index != nil {
					idx = *tc.Index
				} else if tc.ID == "" && len(calls) > 0 {
					idx = len(calls) - 1
				}
				for len(calls) <= idx {
					calls = append(calls, &partial{})
				}
				p := calls[idx]
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name += tc.Function.Name
				}
				p.args.WriteString(tc.Function.Arguments)
			}
			if ch.FinishReason != nil {
				resp.FinishReason = *ch.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("llm: stream: %w", err)
	}
	resp.Message = Message{Role: RoleAssistant, Content: content.String()}
	for i, p := range calls {
		if p.name == "" {
			continue
		}
		id := p.id
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		resp.Message.ToolCalls = append(resp.Message.ToolCalls, ToolCall{ID: id, Name: p.name, Arguments: p.args.String()})
	}
	return resp, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
