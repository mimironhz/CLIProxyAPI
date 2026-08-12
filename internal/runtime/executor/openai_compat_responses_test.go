package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

// DeepSeek V4 Pro uses DeepSeek's native /responses endpoint with the client's
// own Responses payload rather than a Chat Completions translation.
func TestOpenAICompatExecutorPostsDeepSeekV4ProResponsesRequest(t *testing.T) {
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
	payload := []byte(`{"model":"deepseek-v4-pro","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	resp, err := executor.Execute(context.Background(), responsesAuth(server.URL), cliproxyexecutor.Request{
		Model:   "deepseek-v4-pro",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gotPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses", gotPath)
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

func TestOpenAICompatExecutorStreamsDeepSeekDelegatedAgentMessageAsUserInput(t *testing.T) {
	var gotBody []byte
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var errRead error
		gotBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_delegate\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"m1\",\"status\":\"completed\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"payload received\"}]}],\"usage\":{\"input_tokens\":2048,\"output_tokens\":2,\"total_tokens\":2050}}}\n\n"))
	}))
	defer server.Close()

	envelope := "Message Type: NEW_TASK\nTask name: /root/phase2_service_fallback\nSender: /root\nPayload:\n"
	taskBody := "BEGIN_DELEGATED_TASK\n" + strings.Repeat(
		"Inspect the bounded service lane, preserve unrelated work, record structural evidence, and report the exact verification result.\n",
		96,
	) + "END_DELEGATED_TASK"
	delegation := "<codex_delegation>\n" +
		"  <source_thread_id>019fe9b7-a4c3-7820-be88-ec9e4f83b85d</source_thread_id>\n" +
		"  <input>\n" + taskBody + "\n  </input>\n" +
		"</codex_delegation>"
	followupBody := "This is the same-worker follow-up delivery proof. Reply exactly DEEPSEEK_FOLLOWUP_VISIBLE_20260810."
	opaqueAgentContent := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x00, 0xff, 0x81, 0x42}, 48))
	payload, errMarshal := json.Marshal(map[string]any{
		"model":  "deepseek-v4-flash",
		"stream": true,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "existing conversation"}}},
			map[string]any{"type": "reasoning", "encrypted_content": "provider-opaque-reasoning"},
			map[string]any{
				"type":      "agent_message",
				"id":        "amsg_delegate_fixture",
				"author":    "/root",
				"recipient": "/root/phase2_service_fallback",
				"content": []any{
					map[string]any{"type": "input_text", "text": envelope},
					map[string]any{"type": "encrypted_content", "encrypted_content": delegation},
					map[string]any{"type": "encrypted_content", "encrypted_content": opaqueAgentContent},
				},
				"internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn_delegate_fixture"},
			},
			map[string]any{
				"type":      "agent_message",
				"id":        "amsg_followup_fixture",
				"author":    "/root",
				"recipient": "/root/phase2_service_fallback",
				"content": []any{
					map[string]any{"type": "input_text", "text": envelope},
					map[string]any{"type": "encrypted_content", "encrypted_content": followupBody},
				},
				"internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn_followup_fixture"},
			},
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal delegated request fixture: %v", errMarshal)
	}

	// Keep the optional broad optimization disabled. The compatibility boundary
	// must still make this exact delegated turn visible to DeepSeek.
	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := responsesAuth("http://api.deepseek.com/v1")
	auth.ProxyURL = server.URL
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
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
	}

	if gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", gotPath)
	}
	message := gjson.GetBytes(gotBody, "input.2")
	if got := message.Get("type").String(); got != "message" {
		t.Fatalf("delegated item type = %q, want message", got)
	}
	if got := message.Get("role").String(); got != "user" {
		t.Fatalf("delegated item role = %q, want user", got)
	}
	if got := message.Get("content.0.text").String(); got != envelope {
		t.Fatalf("delegation envelope changed: got length %d, want %d", len(got), len(envelope))
	}
	if got := message.Get("content.1.text").String(); got != delegation {
		t.Fatalf("delegated task changed: got length %d, want %d", len(got), len(delegation))
	}
	if got := message.Get("content.1.type").String(); got != "input_text" {
		t.Fatalf("delegated task content type = %q, want input_text", got)
	}
	if got := message.Get("content.#").Int(); got != 2 {
		t.Fatalf("delegated content parts = %d, want 2", got)
	}
	if strings.Contains(string(gotBody), opaqueAgentContent) {
		t.Fatal("opaque encrypted_content was copied into the outbound DeepSeek request")
	}
	followup := gjson.GetBytes(gotBody, "input.3")
	if got := followup.Get("type").String(); got != "message" {
		t.Fatalf("follow-up item type = %q, want message", got)
	}
	if got := followup.Get("role").String(); got != "user" {
		t.Fatalf("follow-up role = %q, want user", got)
	}
	if got := followup.Get("content.1.type").String(); got != "input_text" {
		t.Fatalf("follow-up content type = %q, want input_text", got)
	}
	if got := followup.Get("content.1.text").String(); got != followupBody {
		t.Fatalf("follow-up changed: got %q, want %q", got, followupBody)
	}
}

func TestPrepareDeepSeekCodexInputScope(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"input":[{"type":"agent_message","content":[{"type":"input_text","text":"task"},{"type":"encrypted_content","encrypted_content":"opaque"}]}],"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","description":"unchanged","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}},{"type":"function","name":"followup_task","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}},{"type":"function","name":"send_message","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}}]}]}`)
	for _, tt := range []struct {
		name     string
		baseURL  string
		wantType string
	}{
		{name: "DeepSeek", baseURL: "https://api.deepseek.com/v1", wantType: "message"},
		{name: "other provider", baseURL: "https://example.com/v1", wantType: "agent_message"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := prepareDeepSeekCodexInput(tt.baseURL, payload)
			if gotType := gjson.GetBytes(got, "input.0.type").String(); gotType != tt.wantType {
				t.Fatalf("input type = %q, want %q; payload=%s", gotType, tt.wantType, got)
			}
			if tt.wantType == "message" {
				if gjson.GetBytes(got, "input.0.content.#").Int() != 1 || strings.Contains(string(got), "opaque") {
					t.Fatalf("DeepSeek child input exposed opaque content: %s", got)
				}
				for _, path := range []string{
					"tools.0.tools.0.parameters.properties.message.encrypted",
					"tools.0.tools.1.parameters.properties.message.encrypted",
					"tools.0.tools.2.parameters.properties.message.encrypted",
				} {
					if gjson.GetBytes(got, path).Exists() {
						t.Fatalf("DeepSeek parent schema retained encryption marker at %s: %s", path, got)
					}
				}
			} else if string(got) != string(payload) {
				t.Fatalf("unrelated provider payload changed: %s", got)
			}
		})
	}
}

func TestOpenAICompatExecutorDeepSeekDelegatedPayloadIntegration(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("CLIPROXY_DEEPSEEK_E2E_API_KEY"))
	if apiKey == "" {
		t.Skip("CLIPROXY_DEEPSEEK_E2E_API_KEY is unset")
	}

	const sentinel = "DEEPSEEK_FOLLOWUP_PAYLOAD_VISIBLE_7F3A9C"
	envelope := "Message Type: NEW_TASK\nTask name: /root/deepseek_payload_probe\nSender: /root\nPayload:\n"
	taskBody := "BEGIN_DELEGATED_TASK\n" + strings.Repeat(
		"This is retained context for a delegated worker payload visibility probe. Read every line before answering.\n",
		48,
	) + "Retain this initial task context for the next message.\nEND_DELEGATED_TASK"
	delegation := "<codex_delegation>\n" +
		"  <source_thread_id>019fe9b7-a4c3-7820-be88-ec9e4f83b85d</source_thread_id>\n" +
		"  <input>\n" + taskBody + "\n  </input>\n" +
		"</codex_delegation>"
	opaqueAgentContent := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x00, 0xff, 0x81, 0x42}, 48))
	payload, errMarshal := json.Marshal(map[string]any{
		"model":  "deepseek-v4-flash",
		"stream": true,
		"input": []any{map[string]any{
			"type":      "agent_message",
			"id":        "amsg_deepseek_e2e_probe",
			"author":    "/root",
			"recipient": "/root/deepseek_payload_probe",
			"content": []any{
				map[string]any{"type": "input_text", "text": envelope},
				map[string]any{"type": "encrypted_content", "encrypted_content": delegation},
				map[string]any{"type": "encrypted_content", "encrypted_content": opaqueAgentContent},
			},
			"internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn_deepseek_e2e_probe"},
		}, map[string]any{
			"type":      "agent_message",
			"id":        "amsg_deepseek_e2e_followup",
			"author":    "/root",
			"recipient": "/root/deepseek_payload_probe",
			"content": []any{
				map[string]any{"type": "input_text", "text": envelope},
				map[string]any{"type": "encrypted_content", "encrypted_content": "This is the follow-up payload. Reply with exactly this token and no other text: " + sentinel},
			},
			"internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn_deepseek_e2e_followup"},
		}},
	})
	if errMarshal != nil {
		t.Fatalf("marshal delegated request: %v", errMarshal)
	}

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := responsesAuth("https://api.deepseek.com/v1")
	auth.Attributes["api_key"] = apiKey
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

	var output strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if !strings.Contains(output.String(), sentinel) {
		t.Fatalf("DeepSeek response did not contain delegated payload sentinel; response bytes=%d", output.Len())
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
