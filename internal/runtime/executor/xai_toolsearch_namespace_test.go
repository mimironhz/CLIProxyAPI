package executor

import (
	"testing"
)

// Tools the client loads through tool_search arrive only inside
// tool_search_output items, and applyXAIToolSearchRequest merges them into the
// tools array already flattened to their qualified "<namespace>__<child>" name.
// The (namespace, short name) mapping therefore has to be recorded from the
// tool_search_output item itself. Without it Grok's reply reaches Codex Desktop
// as codex_app__send_message_to_thread and is rejected with
// "unsupported call: codex_app__send_message_to_thread", which is exactly how a
// delegated task loses its callback to the Root thread.
func TestCollectXAINamespaceToolRefsIncludesToolSearchOutput(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"type":"namespace","name":"codex_app","tools":[
				{"type":"function","name":"read_thread_terminal","parameters":{"type":"object","properties":{}}}
			]}
		],
		"input":[
			{"type":"tool_search_call","id":"ts_1","status":"completed"},
			{"type":"tool_search_output","tools":[
				{"type":"namespace","name":"codex_app","tools":[
					{"type":"function","name":"send_message_to_thread","parameters":{"type":"object","properties":{}}}
				]}
			]}
		]
	}`)

	refs := collectXAINamespaceToolRefs(body)

	ref, ok := refs["codex_app__send_message_to_thread"]
	if !ok {
		t.Fatalf("codex_app__send_message_to_thread missing from refs; got %v", refs)
	}
	if ref.namespace != "codex_app" || ref.name != "send_message_to_thread" {
		t.Fatalf("ref = %+v, want {codex_app send_message_to_thread}", ref)
	}
	// The eagerly declared sibling must keep working.
	if _, ok = refs["codex_app__read_thread_terminal"]; !ok {
		t.Fatalf("codex_app__read_thread_terminal missing from refs; got %v", refs)
	}
}

// The response filter drops calls it cannot attribute to a client-declared tool,
// so loaded tools must register as client-declared too.
func TestCollectXAIClientDeclaredToolKeysIncludesToolSearchOutput(t *testing.T) {
	body := []byte(`{
		"tools":[],
		"input":[
			{"type":"tool_search_output","tools":[
				{"type":"namespace","name":"codex_app","tools":[
					{"type":"function","name":"send_message_to_thread","parameters":{"type":"object","properties":{}}}
				]}
			]}
		]
	}`)

	keys := collectXAIClientDeclaredToolKeys(body)

	want := xaiClientToolKey{namespace: "codex_app", name: "send_message_to_thread", toolType: xaiFunctionToolType}
	if _, ok := keys[want]; !ok {
		t.Fatalf("%+v missing from client-declared keys; got %v", want, keys)
	}
}
