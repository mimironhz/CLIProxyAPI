package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// Chat Completions upstreams such as DeepSeek reject Codex's hosted tool_search
// declaration, so the executor forwards it as a plain function and converts the
// model's call back into a tool_search_call item for Desktop's own loader.
func TestOpenAICompatExecutorFlattensAndRestoresToolSearch(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"tool_search","arguments":"{\"query\":\"send_message_to_thread\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL + "/v1",
		"api_key":  "test",
	}}
	payload := []byte(`{"model":"deepseek-v4-flash","input":[{"role":"user","content":"hi"}],"tools":[{"type":"tool_search"}]}`)
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	forwarded := gjson.GetBytes(gotBody, "tools")
	if !forwarded.IsArray() || len(forwarded.Array()) != 1 {
		t.Fatalf("forwarded tools = %s, want a single entry", forwarded.Raw)
	}
	tool := forwarded.Array()[0]
	if tool.Get("type").String() != "function" {
		t.Errorf("forwarded tool type = %q, want function", tool.Get("type").String())
	}
	if name := tool.Get("function.name").String(); name != "tool_search" {
		t.Errorf("forwarded tool name = %q, want tool_search", name)
	}

	output := gjson.GetBytes(resp.Payload, "output")
	if !output.IsArray() {
		t.Fatalf("response output = %s, want an array", output.Raw)
	}
	restored := false
	for _, item := range output.Array() {
		if item.Get("type").String() == "tool_search_call" {
			restored = true
			if execution := item.Get("execution").String(); execution != "client" {
				t.Errorf("tool_search_call execution = %q, want client", execution)
			}
			if query := item.Get("arguments.query").String(); query != "send_message_to_thread" {
				t.Errorf("tool_search_call arguments.query = %q, want send_message_to_thread", query)
			}
		}
	}
	if !restored {
		t.Fatalf("response did not carry a tool_search_call item: %s", string(resp.Payload))
	}
}
