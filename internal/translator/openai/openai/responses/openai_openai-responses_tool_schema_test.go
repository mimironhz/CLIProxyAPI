package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func chatToolParameters(t *testing.T, out []byte, name string) gjson.Result {
	t.Helper()
	tools := gjson.GetBytes(out, "tools")
	if !tools.IsArray() {
		t.Fatalf("tools is not an array; out=%s", out)
	}
	for _, tool := range tools.Array() {
		if tool.Get("function.name").String() == name {
			return tool.Get("function.parameters")
		}
	}
	t.Fatalf("tool %q not found; out=%s", name, out)
	return gjson.Result{}
}

// codex_app.automation_update declares a bare root union with no "type". The
// Responses API accepts it, Chat Completions upstreams do not: Kimi answers 400
// `tools.function.parameters.type is required and must be "object"` and the turn
// dies. The tool is deferred, so it only reaches the wire after tool_search
// loads it, which is why the failure surfaces mid-conversation.
func TestResponsesToChatStampsRootTypeOnRootUnionParameters(t *testing.T) {
	payload := []byte(`{
		"model": "k3",
		"tools": [
			{"type":"namespace","name":"codex_app","tools":[
				{"type":"function","name":"automation_update","parameters":{
					"oneOf":[
						{"type":"object","properties":{"mode":{"type":"string"}},"required":["mode"]},
						{"oneOf":[{"type":"object","properties":{"name":{"type":"string"}}}]}
					],
					"$defs":{"__schema0":{"type":"string"}}
				}}
			]}
		],
		"input": [{"type":"message","role":"user","content":"hi"}]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("k3", payload, false)
	parameters := chatToolParameters(t, out, "codex_app__automation_update")

	if got := parameters.Get("type").String(); got != "object" {
		t.Fatalf("parameters.type = %q, want object; parameters=%s", got, parameters.Raw)
	}
	// The original schema has to survive intact, or the model loses every branch
	// it is supposed to choose between.
	if got := len(parameters.Get("oneOf").Array()); got != 2 {
		t.Fatalf("oneOf branches = %d, want 2; parameters=%s", got, parameters.Raw)
	}
	if !parameters.Get(`$defs.__schema0`).Exists() {
		t.Fatalf("$defs dropped; parameters=%s", parameters.Raw)
	}
}

// A schema that already declares its root type must pass through byte-identical,
// including the non-string form some tools use.
func TestResponsesToChatLeavesTypedParametersUntouched(t *testing.T) {
	payload := []byte(`{
		"model": "k3",
		"tools": [
			{"type":"function","name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}},
			{"type":"function","name":"multi","parameters":{"type":["object"],"properties":{}}}
		],
		"input": [{"type":"message","role":"user","content":"hi"}]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("k3", payload, false)

	shell := chatToolParameters(t, out, "shell")
	if got := shell.Get("type").String(); got != "object" {
		t.Fatalf("shell parameters.type = %q, want object", got)
	}
	if got := shell.Get("required.0").String(); got != "cmd" {
		t.Fatalf("shell required = %s, want [cmd]", shell.Get("required").Raw)
	}

	multi := chatToolParameters(t, out, "multi")
	if got := multi.Get("type").Raw; got != `["object"]` {
		t.Fatalf(`multi parameters.type = %s, want ["object"]`, got)
	}
}

// Tools loaded through tool_search are merged into the tools array by the
// executor before translation, and Codex Desktop also delivers tools through an
// additional_tools input item. Both sources reach the same conversion, so both
// have to be normalized.
func TestResponsesToChatStampsRootTypeFromAdditionalTools(t *testing.T) {
	payload := []byte(`{
		"model": "k3",
		"tools": [],
		"input": [
			{"type":"additional_tools","tools":[
				{"type":"function","name":"deferred","parameters":{"anyOf":[{"type":"object","properties":{}}]}}
			]},
			{"type":"message","role":"user","content":"hi"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("k3", payload, false)

	if got := chatToolParameters(t, out, "deferred").Get("type").String(); got != "object" {
		t.Fatalf("parameters.type = %q, want object", got)
	}
}

// A tool that declares no parameters keeps the empty default rather than growing
// a schema the client never sent.
func TestResponsesToChatKeepsMissingParametersEmpty(t *testing.T) {
	payload := []byte(`{
		"model": "k3",
		"tools": [{"type":"function","name":"noargs"}],
		"input": [{"type":"message","role":"user","content":"hi"}]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("k3", payload, false)

	if got := chatToolParameters(t, out, "noargs").Raw; got != "{}" {
		t.Fatalf("parameters = %s, want {}", got)
	}
}
