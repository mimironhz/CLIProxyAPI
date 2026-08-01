package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

func toolNames(raw []byte) []string {
	tools := gjson.GetBytes(raw, "tools")
	if !tools.IsArray() {
		return nil
	}
	names := make([]string, 0, len(tools.Array()))
	for _, tool := range tools.Array() {
		if name := tool.Get("name").String(); name != "" {
			names = append(names, name)
			continue
		}
		names = append(names, tool.Get("type").String())
	}
	return names
}

func namespaceChildren(raw []byte, namespace string) []string {
	var out []string
	gjson.GetBytes(raw, "tools").ForEach(func(_, tool gjson.Result) bool {
		if tool.Get("name").String() != namespace {
			return true
		}
		tool.Get("tools").ForEach(func(_, child gjson.Result) bool {
			out = append(out, child.Get("name").String())
			return true
		})
		return false
	})
	return out
}

// Codex declares tool_search as a hosted tool type, which Chat Completions
// upstreams cannot represent. It has to reach the model as a plain function or
// the deferred codex_app children are unreachable.
func TestPrepareResponsesToolSearchHoistsHostedTool(t *testing.T) {
	payload := []byte(`{"tools":[{"type":"tool_search"},{"type":"function","name":"shell"}],"input":[]}`)

	out := PrepareResponsesToolSearch(payload)

	names := toolNames(out)
	if len(names) != 2 || names[0] != "shell" || names[1] != "tool_search" {
		t.Fatalf("tools = %v, want [shell tool_search]", names)
	}
	gjson.GetBytes(out, "tools").ForEach(func(_, tool gjson.Result) bool {
		if tool.Get("name").String() == "tool_search" && tool.Get("type").String() != "function" {
			t.Fatalf("tool_search type = %q, want function", tool.Get("type").String())
		}
		return true
	})
}

// A namespace child would be forwarded under its qualified
// "<namespace>__tool_search" name, which the response path cannot match, so it
// must be hoisted to a single flat top-level function.
func TestPrepareResponsesToolSearchHoistsNamespacedAndDedupes(t *testing.T) {
	payload := []byte(`{"tools":[
		{"type":"tool_search"},
		{"type":"namespace","name":"codex_app","tools":[
			{"type":"tool_search"},
			{"type":"function","name":"read_thread_terminal"}
		]}
	],"input":[]}`)

	out := PrepareResponsesToolSearch(payload)

	count := 0
	for _, name := range toolNames(out) {
		if name == "tool_search" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("tool_search declared %d times, want 1; tools=%v", count, toolNames(out))
	}
	if children := namespaceChildren(out, "codex_app"); len(children) != 1 || children[0] != "read_thread_terminal" {
		t.Fatalf("codex_app children = %v, want [read_thread_terminal]", children)
	}
}

// Tools the client loads arrive only inside tool_search_output items. The loaded
// codex_app namespace carries the deferred thread tools while the base request
// declares the same namespace with its eager children; replacing rather than
// merging would drop one set, leaving send_message_to_thread unreachable.
func TestPrepareResponsesToolSearchMergesNamespaceChildren(t *testing.T) {
	payload := []byte(`{"tools":[
		{"type":"namespace","name":"codex_app","tools":[
			{"type":"function","name":"read_thread_terminal"}
		]}
	],"input":[
		{"type":"tool_search_call","id":"ts_1","status":"completed"},
		{"type":"tool_search_output","tools":[
			{"type":"namespace","name":"codex_app","tools":[
				{"type":"function","name":"send_message_to_thread","defer_loading":true},
				{"type":"function","name":"wait_threads","defer_loading":true}
			]}
		]},
		{"type":"message","role":"user","content":"hi"}
	]}`)

	out := PrepareResponsesToolSearch(payload)

	children := namespaceChildren(out, "codex_app")
	want := map[string]bool{"read_thread_terminal": false, "send_message_to_thread": false, "wait_threads": false}
	for _, child := range children {
		if _, ok := want[child]; !ok {
			t.Fatalf("unexpected codex_app child %q; children=%v", child, children)
		}
		want[child] = true
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("codex_app child %q missing; children=%v", name, children)
		}
	}
}

// The round-trip items are client-side orchestration with no Chat Completions
// equivalent, and the tools they carried are already merged into the array.
func TestPrepareResponsesToolSearchDropsRoundTripItems(t *testing.T) {
	payload := []byte(`{"tools":[],"input":[
		{"type":"tool_search_call","id":"ts_1","status":"completed"},
		{"type":"tool_search_output","tools":[{"type":"function","name":"js"}]},
		{"type":"message","role":"user","content":"hi"}
	]}`)

	out := PrepareResponsesToolSearch(payload)

	gjson.GetBytes(out, "input").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "tool_search_call", "tool_search_output":
			t.Fatalf("round-trip item survived: %s", item.Raw)
		}
		return true
	})
	if names := toolNames(out); len(names) != 1 || names[0] != "js" {
		t.Fatalf("tools = %v, want [js]", names)
	}
}

// Codex Desktop only runs its deferred-tool loader for a tool_search_call item;
// a function_call named tool_search is inert.
func TestRestoreToolSearchStreamChunkConvertsCall(t *testing.T) {
	chunk := []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"tool_search","arguments":"{\"query\":\"send_message_to_thread\"}"}}`)

	out := RestoreToolSearchStreamChunk(chunk)

	item := gjson.GetBytes(out[len("data: "):], "item")
	if got := item.Get("type").String(); got != "tool_search_call" {
		t.Fatalf("item type = %q, want tool_search_call; out=%s", got, string(out))
	}
	if got := item.Get("execution").String(); got != "client" {
		t.Fatalf("execution = %q, want client", got)
	}
	if got := item.Get("status").String(); got != "completed" {
		t.Fatalf("status = %q, want completed", got)
	}
	if got := item.Get("arguments.query").String(); got != "send_message_to_thread" {
		t.Fatalf("arguments.query = %q, want send_message_to_thread", got)
	}
	if got := item.Get("call_id").String(); got != "call_1" {
		t.Fatalf("call_id = %q, want call_1", got)
	}
}

func TestRestoreToolSearchStreamChunkLeavesOtherCalls(t *testing.T) {
	chunk := []byte(`data: {"type":"response.output_item.done","item":{"type":"function_call","name":"shell","arguments":"{}"}}`)

	if out := RestoreToolSearchStreamChunk(chunk); string(out) != string(chunk) {
		t.Fatalf("chunk rewritten unexpectedly:\n got=%s\nwant=%s", string(out), string(chunk))
	}
}

func TestRestoreToolSearchResponseConvertsCompletedOutput(t *testing.T) {
	body := []byte(`{"output":[
		{"type":"function_call","name":"shell","arguments":"{}"},
		{"type":"function_call","id":"fc_2","name":"tool_search","arguments":"{\"query\":\"threads\"}"}
	]}`)

	out := RestoreToolSearchResponse(body)

	if got := gjson.GetBytes(out, "output.0.type").String(); got != "function_call" {
		t.Fatalf("output.0 type = %q, want function_call", got)
	}
	if got := gjson.GetBytes(out, "output.1.type").String(); got != "tool_search_call" {
		t.Fatalf("output.1 type = %q, want tool_search_call; out=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "output.1.arguments.query").String(); got != "threads" {
		t.Fatalf("arguments.query = %q, want threads", got)
	}
}
