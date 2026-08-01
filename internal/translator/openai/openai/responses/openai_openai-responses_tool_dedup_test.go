package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func chatToolNames(t *testing.T, out []byte) []string {
	t.Helper()
	tools := gjson.GetBytes(out, "tools")
	if !tools.IsArray() {
		return nil
	}
	names := make([]string, 0, len(tools.Array()))
	for _, tool := range tools.Array() {
		names = append(names, tool.Get("function.name").String())
	}
	return names
}

func countName(names []string, want string) int {
	total := 0
	for _, name := range names {
		if name == want {
			total++
		}
	}
	return total
}

// Codex Desktop declares tools in both the top-level "tools" field and an
// "additional_tools" input item. Merging both without deduping produced repeated
// function names, which strict Chat Completions upstreams reject outright —
// Kimi responds 400 "function name request_user_input is duplicated".
func TestResponsesToChatDedupesToolsAcrossSources(t *testing.T) {
	payload := []byte(`{
		"model": "k3",
		"tools": [
			{"type":"function","name":"request_user_input","parameters":{"type":"object","properties":{}}},
			{"type":"function","name":"shell","parameters":{"type":"object","properties":{}}}
		],
		"input": [
			{"type":"additional_tools","tools":[
				{"type":"function","name":"request_user_input","parameters":{"type":"object","properties":{}}}
			]},
			{"type":"message","role":"user","content":"hi"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("k3", payload, false)
	names := chatToolNames(t, out)

	if got := countName(names, "request_user_input"); got != 1 {
		t.Fatalf("request_user_input appears %d times, want 1; tools=%v", got, names)
	}
	if got := countName(names, "shell"); got != 1 {
		t.Fatalf("shell appears %d times, want 1; tools=%v", got, names)
	}
	if len(names) != 2 {
		t.Fatalf("tools length = %d, want 2; tools=%v", len(names), names)
	}
}

// A namespace child qualifies to "<namespace>__<child>", which can collide with
// a top-level tool that already carries the qualified name. This is the shape
// Codex Desktop sends for its collaboration tools.
func TestResponsesToChatDedupesNamespaceAgainstQualifiedTopLevel(t *testing.T) {
	payload := []byte(`{
		"model": "k3",
		"tools": [
			{"type":"function","name":"collaboration__spawn_agent","parameters":{"type":"object","properties":{}}},
			{"type":"namespace","name":"collaboration","tools":[
				{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{}}},
				{"type":"function","name":"wait_agent","parameters":{"type":"object","properties":{}}}
			]}
		],
		"input": [{"type":"message","role":"user","content":"hi"}]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("k3", payload, false)
	names := chatToolNames(t, out)

	if got := countName(names, "collaboration__spawn_agent"); got != 1 {
		t.Fatalf("collaboration__spawn_agent appears %d times, want 1; tools=%v", got, names)
	}
	if got := countName(names, "collaboration__wait_agent"); got != 1 {
		t.Fatalf("collaboration__wait_agent missing or repeated (%d); tools=%v", got, names)
	}
}

// Deduping must not drop genuinely distinct tools.
func TestResponsesToChatKeepsDistinctTools(t *testing.T) {
	payload := []byte(`{
		"model": "k3",
		"tools": [
			{"type":"function","name":"a","parameters":{"type":"object","properties":{}}},
			{"type":"function","name":"b","parameters":{"type":"object","properties":{}}},
			{"type":"namespace","name":"ns","tools":[
				{"type":"function","name":"c","parameters":{"type":"object","properties":{}}}
			]}
		],
		"input": [{"type":"message","role":"user","content":"hi"}]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("k3", payload, false)
	names := chatToolNames(t, out)

	for _, want := range []string{"a", "b", "ns__c"} {
		if countName(names, want) != 1 {
			t.Fatalf("expected tool %q exactly once; tools=%v", want, names)
		}
	}
	if len(names) != 3 {
		t.Fatalf("tools length = %d, want 3; tools=%v", len(names), names)
	}
}

// Tools loaded through tool_search arrive only inside tool_search_output items
// and are forwarded upstream under their qualified "<namespace>__<child>" name.
// If the mapping is not recoverable, the model's reply reaches Codex as
// codex_app__send_message_to_thread and is rejected as an unsupported call,
// which is how a delegated task loses its callback to the Root thread.
func TestSplitQualifiedFunctionCallResolvesToolSearchOutputNamespace(t *testing.T) {
	request := []byte(`{
		"tools":[{"type":"namespace","name":"codex_app","tools":[
			{"type":"function","name":"read_thread_terminal"}
		]}],
		"input":[
			{"type":"tool_search_output","tools":[{"type":"namespace","name":"codex_app","tools":[
				{"type":"function","name":"send_message_to_thread"}
			]}]}
		]
	}`)

	name, namespace := splitResponsesQualifiedFunctionCallFromRequest(request, "codex_app__send_message_to_thread")
	if name != "send_message_to_thread" || namespace != "codex_app" {
		t.Fatalf("split = (%q, %q), want (send_message_to_thread, codex_app)", name, namespace)
	}

	name, namespace = splitResponsesQualifiedFunctionCallFromRequest(request, "codex_app__read_thread_terminal")
	if name != "read_thread_terminal" || namespace != "codex_app" {
		t.Fatalf("eager sibling split = (%q, %q), want (read_thread_terminal, codex_app)", name, namespace)
	}
}
