package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAnthropicTranslation(t *testing.T) {
	body, _, err := translateAnthropic([]byte(`{
 "model":"alpha","max_tokens":40,"system":[{"type":"text","text":"Be brief"}],
 "messages":[{"role":"user","content":[{"type":"text","text":"Look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]},
 {"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{"path":"a"}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"file"}]},{"type":"text","text":"Continue"}]}],
 "tools":[{"name":"read","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"read","disable_parallel_tool_use":true},"stop_sequences":["END"],"temperature":0}`))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(body, &out)
	msgs := out["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages: %s", body)
	}
	if msgs[0].(map[string]any)["content"] != "Be brief" || msgs[3].(map[string]any)["role"] != "tool" {
		t.Fatalf("roles/system: %s", body)
	}
	if !strings.Contains(string(body), "data:image/png;base64,abc") || !strings.Contains(string(body), `\"path\":\"a\"`) {
		t.Fatalf("image/tool: %s", body)
	}
	if out["parallel_tool_calls"] != false || out["temperature"] != float64(0) {
		t.Fatalf("options: %s", body)
	}
}

func TestAnthropicRejectsUnsupportedInput(t *testing.T) {
	for _, body := range []string{`{}`, `{"model":"a","max_tokens":2,"messages":[{"role":"unknown","content":"hi"}]}`, `{"model":"a","max_tokens":2,"messages":[{"role":"user","content":[{"type":"document"}]}]}`, `{"model":"a","max_tokens":2,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1}}`} {
		if _, _, err := translateAnthropic([]byte(body)); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

// The process helper uses this handler to exercise real engine startup and
// HTTP translation rather than replacing the router transport in integration tests.
func anthropicTestBackend(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		http.Error(w, "bad JSON", 400)
		return
	}
	if os.Getenv("ER_FAKE_EXPECT_OUTPUT_CONFIG") == "1" {
		format, _ := in["response_format"].(map[string]any)
		if in["reasoning_effort"] != "high" || format["type"] != "json_schema" || in["output_config"] != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "missing translated output controls"}})
			return
		}
	}
	if os.Getenv("ER_FAKE_EXPECT_THINKING") == "1" {
		kwargs, _ := in["chat_template_kwargs"].(map[string]any)
		effort := "high"
		if in["model"] == "disabled" {
			effort = "none"
		}
		valid := kwargs["enable_thinking"] == (in["model"] != "disabled") && in["reasoning_effort"] == effort && in["thinking"] == nil
		if in["model"] == "enabled" {
			valid = valid && in["reasoning_budget_tokens"] == float64(2048)
		} else {
			valid = valid && in["reasoning_budget_tokens"] == nil
		}
		if !valid {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "incorrect translated thinking controls"}})
			return
		}
	}
	if os.Getenv("ER_FAKE_EXPECT_SYSTEM_MESSAGES") == "1" {
		messages, _ := in["messages"].([]any)
		// Reproduce the strict Jinja template rule from the reported failure.
		for i, item := range messages {
			message, _ := item.(map[string]any)
			if i > 0 && message["role"] == "system" {
				writeJSON(w, 500, map[string]any{"error": map[string]string{"message": "Error: Jinja Exception: System message must be at the beginning."}})
				return
			}
		}
		valid := len(messages) == 4 && in["reasoning_effort"] == "xhigh"
		if valid {
			first, _ := messages[0].(map[string]any)
			assistant, _ := messages[2].(map[string]any)
			result, _ := messages[3].(map[string]any)
			valid = first["role"] == "system" && first["content"] == "base instructions\ncurrent reminder" && assistant["reasoning_content"] == "earlier reasoning" && result["role"] == "tool"
		}
		if !valid {
			writeJSON(w, 400, map[string]any{"error": map[string]string{"message": "incorrect translated CLI conversation"}})
			return
		}
	}
	if in["model"] == "bad" {
		writeJSON(w, 429, map[string]any{"error": map[string]string{"message": "busy"}})
		return
	}
	if in["stream"] == true {
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"msg_test","choices":[{"delta":{"content":"Hello"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":4}}`,
			`[DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		return
	}
	writeJSON(w, 200, map[string]any{"id": "msg_test", "choices": []any{map[string]any{"message": map[string]any{"content": "Hello", "reasoning_content": "weighing it", "tool_calls": []any{map[string]any{"id": "call_1", "function": map[string]string{"name": "read", "arguments": `{"path":"a"}`}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 8, "completion_tokens": 4}})
}

func TestRouterAutomaticallyDetectsBothAPIs(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}, {Name: "beta"}, {Name: "bad"}}, func(e *Engine) { e.Env = append(e.Env, "ER_FAKE_ANTHROPIC_BACKEND=1") })
	router := NewRouter(mgr)
	for _, anthropic := range []bool{false, true} {
		listing := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/models", nil)
		if anthropic {
			r.Header.Set("anthropic-version", "2023-06-01")
		}
		router.ServeHTTP(listing, r)
		want := `"object": "list"`
		if anthropic {
			want = `"display_name": "alpha"`
		}
		if listing.Code != 200 || !strings.Contains(listing.Body.String(), want) {
			t.Fatal(listing.Body.String())
		}
	}
	request := func(model string, stream bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":%q,"max_tokens":32,"stream":%t,"messages":[{"role":"user","content":"hello"}]}`, model, stream)))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	w := request("alpha", false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"type": "message"`) || !strings.Contains(w.Body.String(), `"stop_reason": "tool_use"`) || w.Header().Get(HeaderUpstream) == "" {
		t.Fatalf("response %d: %s", w.Code, w.Body.String())
	}
	// Engine reasoning becomes a thinking block ahead of the answer.
	if !strings.Contains(w.Body.String(), `"thinking": "weighing it"`) ||
		strings.Index(w.Body.String(), "weighing it") > strings.Index(w.Body.String(), "Hello") {
		t.Fatalf("reasoning: %s", w.Body.String())
	}
	pid := mgr.Status().PID
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"alpha","messages":[]}`))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"choices"`) {
		t.Fatalf("OpenAI: %s", w.Body.String())
	}
	if mgr.Status().PID != pid {
		t.Fatal("changing API restarted the engine")
	}
	// Both client protocols can be in flight without sharing a global mode.
	var clients sync.WaitGroup
	errors := make(chan string, 8)
	for i := 0; i < 8; i++ {
		clients.Add(1)
		go func(anthropic bool) {
			defer clients.Done()
			path, body, marker := "/v1/chat/completions", `{"model":"alpha","messages":[]}`, `"choices"`
			if anthropic {
				path, body, marker = "/v1/messages", `{"model":"alpha","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, `"type": "message"`
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
			if w.Code != 200 || !strings.Contains(w.Body.String(), marker) {
				errors <- w.Body.String()
			}
		}(i%2 == 0)
	}
	clients.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	w = request("beta", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: message_stop") || !strings.Contains(w.Body.String(), "input_json_delta") || !strings.Contains(w.Body.String(), `"input_tokens":8`) {
		t.Fatalf("stream %d: %s", w.Code, w.Body.String())
	}
	if mgr.Status().Model != "beta" {
		t.Fatal("did not switch models")
	}
	w = request("missing", false)
	if w.Code != 404 || !strings.Contains(w.Body.String(), `"type": "not_found_error"`) || w.Header().Get(HeaderUpstream) != "" {
		t.Fatalf("unknown: %s", w.Body.String())
	}
	if mgr.Status().Model != "beta" {
		t.Fatal("unknown model stopped engine")
	}
	w = request("bad", false)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "rate_limit_error") {
		t.Fatalf("upstream error: %s", w.Body.String())
	}
	// Unsupported Anthropic endpoints and incorrect methods must not reach
	// the engine or accidentally return OpenAI-shaped errors.
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/v1/messages/batches", 404}, {"GET", "/v1/messages", 405},
		{"GET", "/v1/messages/count_tokens", 405},
	} {
		w = httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"type": "error"`) || w.Header().Get(HeaderUpstream) != "" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// An unknown OpenAI model still uses the OpenAI error shape after
	// Anthropic traffic on the same router.
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"missing"}`)))
	if w.Code != 404 || !strings.Contains(w.Body.String(), `"code": "model_not_found"`) {
		t.Fatal(w.Code, w.Body.String())
	}

}

func TestAnthropicStreamsReasoningAsThinking(t *testing.T) {
	w := httptest.NewRecorder()
	streamAnthropic(w, strings.NewReader(
		"data: {\"id\":\"msg_test\",\"choices\":[{\"delta\":{\"reasoning_content\":\"weighing\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), "alpha")
	out := w.Body.String()
	if !strings.Contains(out, `"thinking":"weighing"`) || !strings.Contains(out, `"type":"thinking_delta"`) {
		t.Fatalf("no thinking block: %s", out)
	}
	// Reasoning must open the lower-indexed block, before any answer text.
	if strings.Index(out, "thinking_delta") > strings.Index(out, "text_delta") {
		t.Fatalf("thinking follows text: %s", out)
	}
}

func TestAnthropicFailedToolResultSaysSo(t *testing.T) {
	body, _, err := translateAnthropic([]byte(`{"model":"alpha","max_tokens":32,"messages":[
 {"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","is_error":true,"content":"no such file"}]},
 {"role":"assistant","content":[{"type":"tool_use","id":"call_2","name":"read","input":{}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_2","is_error":true}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// A tool message carries no error flag, so the text has to report it.
	if !strings.Contains(string(body), `"Error: no such file"`) || !strings.Contains(string(body), `"Error"`) {
		t.Fatal(string(body))
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	for _, exact := range []bool{true, false} {
		mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
			e.Env = append(e.Env, "ER_FAKE_ANTHROPIC_BACKEND=1")
			if exact {
				e.Env = append(e.Env, "ER_FAKE_TOKENIZER=1")
			}
		})
		w := httptest.NewRecorder()
		// No max_tokens: count_tokens requests never carry one.
		body := `{"model":"alpha","system":"be brief","messages":[{"role":"user","content":"one two three four"}]}`
		NewRouter(mgr).ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body)))
		var out struct {
			InputTokens int `json:"input_tokens"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("exact=%t: %d %s", exact, w.Code, w.Body.String())
		}
		// Six words reach the stand-in tokenizer. The estimate cannot see
		// them, so it must differ while still being a usable number.
		if exact && (out.InputTokens != 6 || w.Header().Get(HeaderUpstream) == "") {
			t.Fatalf("tokenized count %d upstream %q", out.InputTokens, w.Header().Get(HeaderUpstream))
		}
		if !exact && (out.InputTokens <= 0 || w.Header().Get(HeaderUpstream) != "") {
			t.Fatalf("estimated count %d upstream %q", out.InputTokens, w.Header().Get(HeaderUpstream))
		}
	}
}

func TestAnthropicMessagesStillRequireMaxTokens(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, nil)
	w := httptest.NewRecorder()
	body := `{"model":"alpha","messages":[{"role":"user","content":"hi"}]}`
	NewRouter(mgr).ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "max_tokens") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestAnthropicStreamFailure(t *testing.T) {
	for _, data := range []string{"", `data: {bad}` + "\n\n", `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n"} {
		w := httptest.NewRecorder()
		streamAnthropic(w, strings.NewReader(data), "alpha")
		if !strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), "event: message_stop") {
			t.Errorf("false success: %s", w.Body.String())
		}
	}
}

func TestLegacyRouterInterfaceIsIgnored(t *testing.T) {
	for _, mode := range []string{"openai", "anthropic"} {
		c, err := LoadConfig(writeCfg(t, "router_interface: "+mode+"\nengines: []\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Save(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(c.Path())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "router_interface") {
			t.Fatal("saved obsolete setting")
		}
		state, _ := json.Marshal(c)
		if strings.Contains(string(state), "router_interface") {
			t.Fatal("state contains obsolete setting")
		}
	}
}

type flushNotification struct {
	*httptest.ResponseRecorder
	flushed chan struct{}
	once    sync.Once
}

func (w *flushNotification) Flush() {
	w.ResponseRecorder.Flush()
	w.once.Do(func() { close(w.flushed) })
}

func TestAnthropicStreamFlushesBeforeCompletion(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	w := &flushNotification{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	done := make(chan struct{})
	go func() { streamAnthropic(w, reader, "alpha"); close(done) }()
	// Wait for the first flush while the upstream pipe remains open. Read the
	// recorder only after completion to keep the test race-free.
	if _, err := io.WriteString(writer, "data: {\"id\":\"msg_test\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.flushed:
	case <-time.After(2 * time.Second):
		writer.Close()
		t.Fatal("first event was buffered until completion")
	}
	io.WriteString(writer, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	writer.Close()
	<-done
	if !w.Flushed || !strings.Contains(w.Body.String(), "event: message_stop") {
		t.Fatalf("stream not flushed: %s", w.Body.String())
	}
}

func TestAnthropicStreamDoesNotSucceedAfterMalformedTail(t *testing.T) {
	w := httptest.NewRecorder()
	streamAnthropic(w, strings.NewReader("data: {\"id\":\"msg_test\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: broken\n\n"), "alpha")
	if strings.Contains(w.Body.String(), "event: message_stop") || strings.Count(w.Body.String(), "event: error") != 1 {
		t.Fatal(w.Body.String())
	}
}

func TestAnthropicOutputConfig(t *testing.T) {
	schema := `{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`
	for _, tc := range []struct {
		config, effort string
		formatted      bool
	}{
		{`null`, "", false}, {`{}`, "", false},
		{`{"effort":"low"}`, "low", false},
		{`{"effort":"medium"}`, "medium", false},
		{`{"effort":"high"}`, "high", false},
		{`{"effort":"xhigh"}`, "xhigh", false},
		{`{"effort":"max"}`, "xhigh", false},
		{`{"format":{"type":"json_schema","schema":` + schema + `}}`, "", true},
		{`{"effort":"high","format":{"type":"json_schema","schema":` + schema + `}}`, "high", true},
	} {
		t.Run(tc.config, func(t *testing.T) {
			raw := `{"model":"alpha","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"output_config":` + tc.config + `}`
			body, _, err := translateAnthropic([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]json.RawMessage
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatal(err)
			}
			if _, ok := out["output_config"]; ok {
				t.Fatal("Anthropic option leaked to upstream")
			}
			var effort string
			json.Unmarshal(out["reasoning_effort"], &effort)
			if effort != tc.effort {
				t.Fatalf("effort %q, want %q", effort, tc.effort)
			}
			if tc.formatted {
				var format struct {
					Type   string `json:"type"`
					Schema struct {
						Name   string          `json:"name"`
						Strict bool            `json:"strict"`
						Value  json.RawMessage `json:"schema"`
					} `json:"json_schema"`
				}
				if err := json.Unmarshal(out["response_format"], &format); err != nil {
					t.Fatal(err)
				}
				var want, got any
				json.Unmarshal([]byte(schema), &want)
				json.Unmarshal(format.Schema.Value, &got)
				if format.Type != "json_schema" || format.Schema.Name == "" || !format.Schema.Strict || !reflect.DeepEqual(want, got) {
					t.Fatalf("format %s", out["response_format"])
				}
			} else if _, ok := out["response_format"]; ok {
				t.Fatal("unexpected output constraint")
			}
		})
	}
}

func TestAnthropicRejectsInvalidOutputConfig(t *testing.T) {
	for _, config := range []string{`[]`, `"high"`, `{"effort":"extreme"}`, `{"unknown":true}`, `{"format":{"type":"text"}}`, `{"format":{"type":"json_schema"}}`, `{"format":{"type":"json_schema","schema":null}}`, `{"format":{"type":"json_schema","schema":[]}}`, `{"format":{"type":"json_schema","schema":{},"unknown":true}}`} {
		raw := `{"model":"alpha","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"output_config":` + config + `}`
		if _, _, err := translateAnthropic([]byte(raw)); err == nil || !strings.Contains(err.Error(), "output_config") {
			t.Errorf("config %s: %v", config, err)
		}
	}
}

func TestAnthropicOutputConfigReachesEngine(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
		e.Env = append(e.Env, "ER_FAKE_ANTHROPIC_BACKEND=1", "ER_FAKE_EXPECT_OUTPUT_CONFIG=1")
	})
	router := NewRouter(mgr)
	for _, stream := range []bool{false, true} {
		raw := fmt.Sprintf(`{"model":"alpha","max_tokens":64,"stream":%t,"messages":[{"role":"user","content":"hello"}],"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object"}}}}`, stream)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(raw)))
		want := `"type": "message"`
		if stream {
			want = "event: message_stop"
		}
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("stream=%t: %d %s", stream, w.Code, w.Body.String())
		}
	}
}

func TestAnthropicThinkingControls(t *testing.T) {
	for _, tc := range []struct {
		thinking, effort string
		enabled          bool
		budget           int
		present          bool
	}{
		{`null`, "high", false, 0, false},
		{`{"type":"enabled","budget_tokens":2048}`, "high", true, 2048, true},
		{`{"type":"enabled","budget_tokens":1024,"display":"omitted"}`, "high", true, 1024, true},
		{`{"type":"adaptive"}`, "high", true, 0, true},
		{`{"type":"adaptive","display":"summarized"}`, "high", true, 0, true},
		{`{"type":"disabled"}`, "none", false, 0, true},
	} {
		t.Run(tc.thinking, func(t *testing.T) {
			raw := `{"model":"alpha","max_tokens":4096,"messages":[{"role":"user","content":"hello"}],"output_config":{"effort":"high"},"thinking":` + tc.thinking + `}`
			body, _, err := translateAnthropic([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			json.Unmarshal(body, &out)
			if _, ok := out["thinking"]; ok {
				t.Fatal("Anthropic thinking leaked to engine")
			}
			if out["reasoning_effort"] != tc.effort {
				t.Fatalf("effort: %s", body)
			}
			if tc.present {
				kwargs, ok := out["chat_template_kwargs"].(map[string]any)
				if !ok || kwargs["enable_thinking"] != tc.enabled {
					t.Fatalf("thinking control: %s", body)
				}
			} else if _, ok := out["chat_template_kwargs"]; ok {
				t.Fatal("changed unspecified thinking")
			}
			if tc.budget > 0 {
				if out["reasoning_budget_tokens"] != float64(tc.budget) {
					t.Fatalf("budget: %s", body)
				}
			} else if _, ok := out["reasoning_budget_tokens"]; ok {
				t.Fatal("unexpected reasoning budget")
			}
		})
	}
}

func TestAnthropicThinkingValidation(t *testing.T) {
	for _, thinking := range []string{`{}`, `[]`, `"adaptive"`, `{"type":"unknown"}`, `{"type":"enabled"}`, `{"type":"enabled","budget_tokens":1023}`, `{"type":"enabled","budget_tokens":4096}`, `{"type":"enabled","budget_tokens":-1}`, `{"type":"adaptive","budget_tokens":1024}`, `{"type":"disabled","budget_tokens":1024}`, `{"type":"adaptive","display":"unknown"}`} {
		raw := `{"model":"alpha","max_tokens":4096,"messages":[{"role":"user","content":"hello"}],"thinking":` + thinking + `}`
		if _, _, err := translateAnthropic([]byte(raw)); err == nil || !strings.Contains(err.Error(), "thinking") {
			t.Errorf("thinking %s: %v", thinking, err)
		}
	}
}

func TestAnthropicThinkingHistory(t *testing.T) {
	raw := `{"model":"alpha","max_tokens":4096,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"earlier reasoning","signature":"opaque-signature"},{"type":"redacted_thinking","data":"opaque-redacted"},{"type":"tool_use","id":"call1","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"result"}]}]}`
	body, _, err := translateAnthropic([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []map[string]any `json:"messages"`
	}
	json.Unmarshal(body, &out)
	if len(out.Messages) != 2 || out.Messages[0]["reasoning_content"] != "earlier reasoning" || out.Messages[0]["content"] != nil || out.Messages[1]["role"] != "tool" {
		t.Fatalf("history: %s", body)
	}
	if strings.Contains(string(body), "opaque-") {
		t.Fatal("opaque Claude data reached engine")
	}
	raw = `{"model":"alpha","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"thinking","thinking":"text"}]}]}`
	if _, _, err := translateAnthropic([]byte(raw)); err == nil {
		t.Fatal("user thinking block accepted")
	}
}

func TestAnthropicThinkingReachesEngine(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "enabled"}, {Name: "adaptive"}, {Name: "disabled"}}, func(e *Engine) { e.Env = append(e.Env, "ER_FAKE_ANTHROPIC_BACKEND=1", "ER_FAKE_EXPECT_THINKING=1") })
	router := NewRouter(mgr)
	for _, mode := range []string{"enabled", "adaptive", "disabled"} {
		for _, stream := range []bool{false, true} {
			thinking := `{"type":"` + mode + `"}`
			if mode == "enabled" {
				thinking = `{"type":"enabled","budget_tokens":2048}`
			}
			raw := fmt.Sprintf(`{"model":%q,"max_tokens":4096,"stream":%t,"messages":[{"role":"user","content":"hello"}],"thinking":%s,"output_config":{"effort":"high"}}`, mode, stream, thinking)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(raw)))
			marker := `"type": "message"`
			if stream {
				marker = "event: message_stop"
			}
			if w.Code != 200 || !strings.Contains(w.Body.String(), marker) {
				t.Fatalf("mode=%s stream=%t: %d %s", mode, stream, w.Code, w.Body.String())
			}
		}
	}
}

func TestAnthropicSystemMessages(t *testing.T) {
	raw := `{"model":"alpha","max_tokens":4096,"system":"base instructions","output_config":{"effort":"low"},"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"first question"},{"role":"system","content":[{"type":"text","text":"new instruction"},{"type":"text","text":"second line"}]},{"role":"assistant","content":"earlier answer"},{"role":"user","content":"continue"},{"role":"system","content":[],"output_config":{"effort":"medium"}},{"role":"system","content":"","output_config":{"effort":"xhigh"}}]}`
	body, _, err := translateAnthropic([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Effort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"system", "user", "assistant", "user"}
	if len(out.Messages) != len(want) || out.Effort != "xhigh" {
		t.Fatalf("translation: %s", body)
	}
	for i, role := range want {
		if out.Messages[i].Role != role {
			t.Fatalf("message %d role: %s", i, body)
		}
	}
	if out.Messages[0].Content != "base instructions\nnew instruction\nsecond line" || out.Messages[1].Content != "first question" || out.Messages[2].Content != "earlier answer" || out.Messages[3].Content != "continue" {
		t.Fatalf("system text: %s", body)
	}
}

func TestAnthropicSystemMessageExpiry(t *testing.T) {
	raw := `{"model":"alpha","max_tokens":4096,"messages":[{"role":"user","content":"old question"},{"role":"system","clear_at":"next_user_message","content":"expired reminder"},{"role":"assistant","content":"old answer"},{"role":"user","content":"new question"},{"role":"system","clear_at":"never","content":"permanent reminder"},{"role":"system","clear_at":"next_user_message","content":"current reminder"}]}`
	body, _, err := translateAnthropic([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "expired reminder") || !strings.Contains(string(body), "permanent reminder") || !strings.Contains(string(body), "current reminder") || strings.Contains(string(body), "clear_at") {
		t.Fatalf("expiry: %s", body)
	}
}

func TestAnthropicSystemEffortRespectsDisabledThinking(t *testing.T) {
	body, _, err := translateAnthropic([]byte(`{"model":"alpha","max_tokens":64,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"},{"role":"system","content":[],"output_config":{"effort":"max"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(body, &out)
	if out["reasoning_effort"] != "none" {
		t.Fatalf("disabled thinking overridden: %s", body)
	}
}

func TestAnthropicInvalidMessageRolesAndControls(t *testing.T) {
	for _, tc := range []struct{ message, detail string }{
		{`{"role":"unexpected","content":"text"}`, `unsupported role "unexpected"`},
		{`{"role":"system","content":[{"type":"image"}]}`, "system content must contain text only"},
		{`{"role":"system","content":[],"output_config":{"format":{"type":"json_schema","schema":{}}}}`, "unsupported system output_config"},
		{`{"role":"system","content":"text","clear_at":"tomorrow"}`, "clear_at"},
		{`{"role":"system","content":"text","clear_at":"next_user_message","output_config":{"effort":"low"}}`, "output_config cannot be combined"},
		{`{"role":"user","content":"text","clear_at":"next_user_message"}`, "require the system role"},
		{`{"role":"assistant","content":"text","output_config":{"effort":"low"}}`, "require the system role"},
	} {
		raw := `{"model":"alpha","max_tokens":64,"messages":[{"role":"user","content":"hi"},` + tc.message + `]}`
		if _, _, err := translateAnthropic([]byte(raw)); err == nil || !strings.Contains(err.Error(), "messages[1]") || !strings.Contains(err.Error(), tc.detail) {
			t.Errorf("%s: %v", tc.message, err)
		}
	}
}

func TestClaudeCLIConversationReachesEngine(t *testing.T) {
	mgr := testManager(t, []*Model{{Name: "alpha"}}, func(e *Engine) {
		e.Env = append(e.Env, "ER_FAKE_ANTHROPIC_BACKEND=1", "ER_FAKE_EXPECT_SYSTEM_MESSAGES=1")
	})
	router := NewRouter(mgr)
	for _, stream := range []bool{false, true} {
		raw := fmt.Sprintf(`{"model":"alpha","max_tokens":4096,"stream":%t,"system":[{"type":"text","text":"base instructions","cache_control":{"type":"ephemeral"}}],"metadata":{"user_id":"test"},"output_config":{"effort":"low"},"thinking":{"type":"adaptive"},"tools":[{"name":"read","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"},"messages":[{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"thinking","thinking":"earlier reasoning","signature":"opaque"},{"type":"tool_use","id":"call1","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"file"}]},{"role":"system","content":"current reminder","clear_at":"next_user_message"},{"role":"system","content":[],"output_config":{"effort":"max"}}]}`, stream)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(raw)))
		want := `"type": "message"`
		if stream {
			want = "event: message_stop"
		}
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("stream=%t: %d %s", stream, w.Code, w.Body.String())
		}
	}
}

func TestCompatibleReasoningEffort(t *testing.T) {
	for _, tc := range []struct{ requested, message, want string }{
		{"high", "Error: Jinja Exception: Unexpected reasoning effort high. Supported types are xhigh (default), medium, and low.", "xhigh"},
		{"xhigh", "Unexpected reasoning effort xhigh. Supported types are high, medium, and low.", "high"},
		{"low", "Unexpected reasoning effort low. Supported types are xhigh and medium.", "medium"},
		{"none", "Unexpected reasoning effort none. Supported types are xhigh and low.", ""},
		{"high", "Unexpected reasoning effort high. Supported types are high, medium and low.", ""},
		{"high", "Unexpected reasoning effort low. Supported types are xhigh and medium.", ""},
		{"high", "Unexpected reasoning effort high. Supported types are unknown.", ""},
		{"high", "out of memory", ""},
	} {
		if got := compatibleEffort(tc.message, tc.requested); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.message, got, tc.want)
		}
	}
}

type anthropicRoundTripFunc func(*http.Request) (*http.Response, error)

func (f anthropicRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnthropicEffortRetry(t *testing.T) {
	const rejection = "While executing CallExpression at line 49\nError: Jinja Exception: Unexpected reasoning effort high. Supported types are xhigh (default), medium, and low."
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			mgr := NewManager(&Config{})
			router := NewRouter(mgr)
			raw := fmt.Sprintf(`{"model":"alpha","max_tokens":4096,"stream":%t,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hello"}]}`, stream)
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(raw))
			in, err := prepareAnthropic(r)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			router.proxy.Transport = anthropicRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				data, err := io.ReadAll(req.Body)
				req.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if int64(len(data)) != req.ContentLength || req.Header.Get("Content-Length") != fmt.Sprint(len(data)) {
					t.Fatal("incorrect retried body length")
				}
				var body map[string]any
				json.Unmarshal(data, &body)
				if body["model"] != "alpha" || body["stream"] != stream || body["chat_template_kwargs"].(map[string]any)["enable_thinking"] != true {
					t.Fatalf("request changed: %s", data)
				}
				if calls == 1 {
					if body["reasoning_effort"] != "high" {
						t.Fatal("original effort lost")
					}
					payload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": rejection}})
					return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
				}
				if calls != 2 || body["reasoning_effort"] != "xhigh" {
					t.Fatalf("retry: %s", data)
				}
				req.Body = io.NopCloser(strings.NewReader(string(data)))
				w := httptest.NewRecorder()
				anthropicTestBackend(w, req)
				return w.Result(), nil
			})
			w := httptest.NewRecorder()
			router.anthropicMessages(w, r, routeTarget{engine: "fake", model: "alpha", port: 8080}, in)
			want := `"type": "message"`
			if stream {
				want = "event: message_stop"
			}
			if calls != 2 || w.Code != 200 || !strings.Contains(w.Body.String(), want) {
				t.Fatalf("calls=%d code=%d: %s", calls, w.Code, w.Body.String())
			}
		})
	}
}

func TestAnthropicEffortRetryIsBounded(t *testing.T) {
	for _, tc := range []struct {
		status          int
		message, effort string
		wantCalls       int
	}{
		{500, "out of memory", "high", 1},
		{429, "Unexpected reasoning effort high. Supported types are xhigh and medium.", "high", 1},
		{500, "Unexpected reasoning effort none. Supported types are xhigh and medium.", "none", 1},
		{500, "Unexpected reasoning effort high. Supported types are xhigh and medium.", "high", 2},
	} {
		mgr := NewManager(&Config{})
		router := NewRouter(mgr)
		calls := 0
		router.proxy.Transport = anthropicRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			req.Body.Close()
			payload, _ := json.Marshal(map[string]any{"error": map[string]string{"message": tc.message}})
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
		})
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"alpha","max_tokens":4096,"messages":[{"role":"user","content":"hello"}],"output_config":{"effort":"high"}}`))
		in, err := prepareAnthropic(r)
		if err != nil {
			t.Fatal(err)
		}
		if tc.effort == "none" {
			data := `{"model":"alpha","reasoning_effort":"none"}`
			r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(data)), nil }
			r.Body, _ = r.GetBody()
		}
		w := httptest.NewRecorder()
		router.anthropicMessages(w, r, routeTarget{engine: "fake", model: "alpha", port: 8080}, in)
		if calls != tc.wantCalls || w.Code != tc.status || !strings.Contains(w.Body.String(), tc.message) {
			t.Fatalf("%+v calls=%d code=%d: %s", tc, calls, w.Code, w.Body.String())
		}
	}
}

func TestAnthropicSystemPrefixCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, system, messages, prefix string
		turns                          int
	}{
		{"late instruction without initial system", `null`, `[{"role":"user","content":"hello"},{"role":"system","content":"late instruction"}]`, "late instruction", 1},
		{"multiple initial and later instructions", `"base"`, `[{"role":"system","content":"first"},{"role":"user","content":"hello"},{"role":"system","content":"second"}]`, "base\nfirst\nsecond", 1},
		{"expired reminder only", `null`, `[{"role":"user","content":"old"},{"role":"system","content":"expired","clear_at":"next_user_message"},{"role":"assistant","content":"reply"},{"role":"user","content":"new"}]`, "", 3},
		{"effort only", `null`, `[{"role":"user","content":"hello"},{"role":"system","content":[],"output_config":{"effort":"medium"}}]`, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"model":"alpha","max_tokens":4096,"system":` + tc.system + `,"messages":` + tc.messages + `}`
			body, _, err := translateAnthropic([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			var out struct {
				Messages []map[string]any `json:"messages"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatal(err)
			}
			systems := 0
			turns := 0
			for i, msg := range out.Messages {
				if msg["role"] == "system" {
					systems++
					if i != 0 || msg["content"] != tc.prefix {
						t.Fatalf("invalid system prefix: %s", body)
					}
				} else {
					turns++
				}
			}
			want := 0
			if tc.prefix != "" {
				want = 1
			}
			if systems != want || turns != tc.turns {
				t.Fatalf("systems=%d turns=%d: %s", systems, turns, body)
			}
		})
	}
}

func TestAnthropicHostHeaderPointsAtEngine(t *testing.T) {
	mgr := NewManager(&Config{})
	router := NewRouter(mgr)
	var gotHost string
	router.proxy.Transport = anthropicRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotHost = req.Host
		req.Body.Close()
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"alpha","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`))
	r.Host = "192.168.0.110:8078"
	in, err := prepareAnthropic(r)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	router.anthropicMessages(w, r, routeTarget{engine: "fake", model: "alpha", port: 8081}, in)
	want := fmt.Sprintf("%s:%d", engineHost, 8081)
	if gotHost != want {
		t.Errorf("got req.Host = %q, want %q", gotHost, want)
	}
}

