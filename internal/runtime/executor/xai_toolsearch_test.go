package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestXAIToolSearchToolBecomesFunction(t *testing.T) {
	raw, changed, ok := normalizeXAITool(gjson.Parse(`{"type":"tool_search"}`), "")
	if !ok {
		t.Fatalf("normalizeXAITool() ok = false, want true")
	}
	if !changed {
		t.Fatalf("normalizeXAITool() changed = false, want true")
	}
	tool := gjson.ParseBytes(raw)
	if got := tool.Get("type").String(); got != "function" {
		t.Fatalf("type = %q, want function", got)
	}
	if got := tool.Get("name").String(); got != "tool_search" {
		t.Fatalf("name = %q, want tool_search", got)
	}
	if !tool.Get("parameters.properties.query").Exists() {
		t.Fatalf("shim is missing the query parameter; tool=%s", tool.Raw)
	}
}

// xAI has no deferred-loading mechanism, so any tool that keeps defer_loading
// stays invisible to the model and it re-runs tool_search forever.
func TestXAIDeferLoadingStripped(t *testing.T) {
	raw, changed, ok := normalizeXAITool(gjson.Parse(`{"type":"function","name":"js","parameters":{"type":"object","properties":{}},"defer_loading":true}`), "")
	if !ok {
		t.Fatalf("normalizeXAITool() ok = false, want true")
	}
	if !changed {
		t.Fatalf("normalizeXAITool() changed = false, want true")
	}
	if gjson.GetBytes(raw, "defer_loading").Exists() {
		t.Fatalf("defer_loading survived normalization; tool=%s", string(raw))
	}
	if got := gjson.GetBytes(raw, "name").String(); got != "js" {
		t.Fatalf("name = %q, want js; tool=%s", got, string(raw))
	}
}

// Tools the client loads via tool_search arrive only inside tool_search_output
// history items and are never re-added to the request tools array.
func TestXAIHarvestsLoadedToolFromHistory(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{}}}],
		"input":[
			{"type":"message","role":"user","content":"run some js"},
			{"type":"tool_search_call","id":"ts_1","status":"completed"},
			{"type":"tool_search_output","tools":[
				{"type":"namespace","name":"mcp__node_repl","tools":[
					{"type":"function","name":"js","parameters":{"type":"object","properties":{}},"defer_loading":true},
					{"type":"function","name":"js_reset","parameters":{"type":"object","properties":{}},"defer_loading":true}
				]}
			]}
		]
	}`)

	got := applyXAIToolSearchRequest(body)

	names := make(map[string]bool)
	for _, tool := range gjson.GetBytes(got, "tools").Array() {
		names[tool.Get("name").String()] = true
		if tool.Get("defer_loading").Exists() {
			t.Fatalf("harvested tool %q kept defer_loading; body=%s", tool.Get("name").String(), string(got))
		}
	}
	if !names["shell"] {
		t.Fatalf("pre-existing tool was dropped; body=%s", string(got))
	}
	for _, want := range []string{"mcp__node_repl__js", "mcp__node_repl__js_reset"} {
		if !names[want] {
			t.Fatalf("harvested tool %q missing from tools; body=%s", want, string(got))
		}
	}

	for _, item := range gjson.GetBytes(got, "input").Array() {
		switch item.Get("type").String() {
		case "tool_search_call", "tool_search_output":
			t.Fatalf("tool_search round-trip item survived; body=%s", string(got))
		}
	}
	if got := gjson.GetBytes(got, "input.#").Int(); got != 1 {
		t.Fatalf("input length = %d, want 1", got)
	}
}

func TestXAIHarvestedToolsDedupedAgainstExisting(t *testing.T) {
	body := []byte(`{
		"tools":[{"type":"function","name":"mcp__node_repl__js","parameters":{"type":"object","properties":{}}}],
		"input":[
			{"type":"tool_search_output","tools":[
				{"type":"namespace","name":"mcp__node_repl","tools":[
					{"type":"function","name":"js","parameters":{"type":"object","properties":{}}}
				]}
			]},
			{"type":"tool_search_output","tools":[
				{"type":"namespace","name":"mcp__node_repl","tools":[
					{"type":"function","name":"js","parameters":{"type":"object","properties":{}}}
				]}
			]}
		]
	}`)

	got := applyXAIToolSearchRequest(body)

	count := 0
	for _, tool := range gjson.GetBytes(got, "tools").Array() {
		if tool.Get("name").String() == "mcp__node_repl__js" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("mcp__node_repl__js count = %d, want 1; body=%s", count, string(got))
	}
}

func TestXAIToolSearchRequestLeavesUnrelatedBodiesAlone(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{}}}],"input":[{"type":"message","role":"user","content":"hi"}]}`)
	got := applyXAIToolSearchRequest(body)
	if string(got) != string(body) {
		t.Fatalf("body was rewritten unnecessarily:\n got=%s\nwant=%s", string(got), string(body))
	}
}
