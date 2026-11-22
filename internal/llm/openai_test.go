package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamTextAndToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("missing auth header")
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["stream"] != true || req["tools"] == nil {
			t.Errorf("unexpected request: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"model":"m1","choices":[{"delta":{"content":"Olá, "}}]}`,
			`{"choices":[{"delta":{"content":"Ana!"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"x\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"ask_user","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewOpenAI(OpenAIConfig{BaseURL: srv.URL + "/", APIKey: "k", Stream: true, StreamUsage: true})
	var deltas []string
	resp, err := c.Chat(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "oi"}},
		Tools: []ToolDef{{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}}}, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "Olá, Ana!" || strings.Join(deltas, "") != "Olá, Ana!" {
		t.Errorf("content = %q deltas=%v", resp.Message.Content, deltas)
	}
	if len(resp.Message.ToolCalls) != 2 || resp.Message.ToolCalls[0].Arguments != `{"path":"x"}` || resp.Message.ToolCalls[1].Name != "ask_user" {
		t.Errorf("tool calls = %+v", resp.Message.ToolCalls)
	}
	if resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 20 || resp.Model != "m1" {
		t.Errorf("usage/model = %+v %s", resp.Usage, resp.Model)
	}
}

func TestNonStreamFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"x","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"write_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	c := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, Stream: true})
	resp, err := c.Chat(context.Background(), Request{Model: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.ToolCalls) != 1 || resp.Usage.InputTokens != 5 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestRetryTransientThenSucceed(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()
	c := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, RetryBackoff: time.Millisecond})
	resp, err := c.Chat(context.Background(), Request{Model: "x"}, nil)
	if err != nil || resp.Message.Content != "ok" || calls != 3 {
		t.Fatalf("resp=%v err=%v calls=%d", resp, err, calls)
	}
}

func TestNoRetryOnClientError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":"bad"}`)
	}))
	defer srv.Close()
	c := NewOpenAI(OpenAIConfig{BaseURL: srv.URL, RetryBackoff: time.Millisecond})
	_, err := c.Chat(context.Background(), Request{Model: "x"}, nil)
	if !errors.Is(err, ErrUnavailable) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestToWireToolMessages(t *testing.T) {
	w := toWire([]Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "1", Name: "x"}}},
		{Role: RoleTool, ToolCallID: "1", Name: "x", Content: "r"},
	})
	if w[0].Content != nil || w[0].ToolCalls[0].Function.Arguments != "{}" {
		t.Errorf("assistant wire = %+v", w[0])
	}
	if w[1].ToolCallID != "1" || *w[1].Content != "r" {
		t.Errorf("tool wire = %+v", w[1])
	}
}
