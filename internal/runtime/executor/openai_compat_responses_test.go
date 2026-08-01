package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func responsesAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":     baseURL,
		"api_key":      "test",
		"upstream_api": config.OpenAICompatAPIResponses,
	}}
}

// A provider configured for the Responses API must reach /responses with the
// client's own Responses payload rather than a Chat Completions translation.
func TestOpenAICompatExecutorPostsResponsesRequest(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","id":"m1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	payload := []byte(`{"model":"deepseek-v4-flash","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	resp, err := executor.Execute(context.Background(), responsesAuth(server.URL+"/v1"), cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gotPath != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses", gotPath)
	}
	if !gjson.GetBytes(gotBody, "input").IsArray() {
		t.Errorf("upstream body lost the Responses input array: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "messages").Exists() {
		t.Errorf("upstream body was translated to Chat Completions: %s", string(gotBody))
	}
	if stream := gjson.GetBytes(gotBody, "stream").Bool(); stream {
		t.Errorf("non-stream call forwarded stream=true: %s", string(gotBody))
	}
	if id := gjson.GetBytes(resp.Payload, "id").String(); id != "resp_1" {
		t.Errorf("response id = %q, want resp_1", id)
	}
}

// Codex declares codex_app.automation_update as a bare root union. The Chat
// Completions translator repairs that; a Responses upstream never reaches it, so
// the executor must, or the upstream rejects the turn outright with 400 "schema
// must be a JSON Schema of 'type: \"object\"'".
func TestOpenAICompatExecutorNormalizesResponsesToolSchemas(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	payload := []byte(`{"model":"deepseek-v4-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"tools":[{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"automation_update","parameters":{"oneOf":[{"type":"object","properties":{}}],"$defs":{}}}]}]}`)
	if _, err := executor.Execute(context.Background(), responsesAuth(server.URL+"/v1"), cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	parameters := gjson.GetBytes(gotBody, `tools.#(name=="codex_app").tools.0.parameters`)
	if got := parameters.Get("type").String(); got != "object" {
		t.Errorf("upstream root type = %q, want object: %s", got, string(gotBody))
	}
	if !parameters.Get("oneOf").IsArray() {
		t.Errorf("upstream lost the original union: %s", string(gotBody))
	}
}

// The knob describes the upstream, not the client. A caller whose format has no
// translator able to read a Responses upstream must keep Chat Completions rather
// than receive raw Responses JSON.
func TestOpenAICompatExecutorKeepsChatCompletionsForChatClients(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := executor.Execute(context.Background(), responsesAuth(server.URL+"/v1"), cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
}

// DeepSeek streams its chain of thought as raw reasoning content and closes
// without a [DONE] marker. The stream must reach the client as summary events
// with the sealed blob Codex-shaped clients actually read.
func TestOpenAICompatExecutorStreamsDeepSeekResponsesReasoning(t *testing.T) {
	events := []string{
		`event: response.created` + "\n" + `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","status":"in_progress"}}`,
		`event: response.output_item.added` + "\n" + `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"r1","status":"in_progress","content":[],"summary":[]}}`,
		`event: response.content_part.added` + "\n" + `data: {"type":"response.content_part.added","content_index":0,"item_id":"r1","output_index":0,"part":{"type":"reasoning_text","text":""}}`,
		`event: response.reasoning_text.delta` + "\n" + `data: {"type":"response.reasoning_text.delta","content_index":0,"item_id":"r1","output_index":0,"delta":"think"}`,
		`event: response.reasoning_text.done` + "\n" + `data: {"type":"response.reasoning_text.done","content_index":0,"item_id":"r1","output_index":0,"text":"think"}`,
		`event: response.content_part.done` + "\n" + `data: {"type":"response.content_part.done","content_index":0,"item_id":"r1","output_index":0,"part":{"type":"reasoning_text","text":"think"}}`,
		`event: response.output_item.done` + "\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"r1","status":"completed","content":[{"type":"reasoning_text","text":"think"}],"summary":[]}}`,
		`event: response.completed` + "\n" + `data: {"type":"response.completed","sequence_number":9,"response":{"id":"resp_1","status":"completed","output":[{"type":"reasoning","id":"r1","status":"completed","content":[{"type":"reasoning_text","text":"think"}],"summary":[]}],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = w.Write([]byte(event + "\n\n"))
		}
	}))
	defer server.Close()

	// Sealing and the summary rewrite are gated on the DeepSeek host, so the
	// upstream must present itself as one. Routing through the test server as an
	// HTTP proxy keeps the configured base URL on that host while the request
	// still lands here.
	auth := responsesAuth("http://api.deepseek.com/v1")
	auth.ProxyURL = server.URL

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	payload := []byte(`{"model":"deepseek-v4-flash","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var collected []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		collected = append(collected, string(chunk.Payload))
	}
	joined := strings.Join(collected, "\n")

	for _, want := range []string{
		"event: response.reasoning_summary_part.added",
		"event: response.reasoning_summary_text.delta",
		"event: response.reasoning_summary_text.done",
		"event: response.reasoning_summary_part.done",
		"event: response.completed",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("stream is missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "response.reasoning_text.delta") {
		t.Errorf("raw reasoning_text events reached the client:\n%s", joined)
	}
	if strings.Contains(joined, "[DONE]") {
		t.Errorf("a Responses stream must not synthesize a [DONE] marker:\n%s", joined)
	}

	var completed string
	for _, chunk := range collected {
		if strings.Contains(chunk, `"type":"response.completed"`) {
			completed = chunk
		}
	}
	if completed == "" {
		t.Fatalf("stream carried no response.completed event:\n%s", joined)
	}
	data := completed[strings.Index(completed, "data: ")+len("data: "):]
	item := gjson.Get(data, "response.output.0")
	if summary := item.Get("summary.0.text").String(); summary != "think" {
		t.Errorf("reasoning summary = %q, want think", summary)
	}
	if encrypted := item.Get("encrypted_content").String(); !strings.HasPrefix(encrypted, "deepseek-reasoning-v1:") {
		t.Errorf("reasoning encrypted_content = %q, want a sealed blob", encrypted)
	}
}

// A Responses stream that ends without a terminal event is a truncated turn, not
// a clean close, so it must surface as an error rather than an empty success.
func TestOpenAICompatExecutorFailsTruncatedResponsesStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	payload := []byte(`{"model":"deepseek-v4-flash","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	result, err := executor.ExecuteStream(context.Background(), responsesAuth(server.URL+"/v1"), cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("a truncated Responses stream completed without an error")
	}
	if !strings.Contains(streamErr.Error(), "terminal event") {
		t.Errorf("stream error = %v, want it to name the missing terminal event", streamErr)
	}
}
