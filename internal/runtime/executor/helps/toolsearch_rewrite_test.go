package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func inputItemTypes(t *testing.T, body []byte) []string {
	t.Helper()
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		t.Fatalf("input is not an array: %s", body)
	}
	types := make([]string, 0, len(input.Array()))
	for _, item := range input.Array() {
		types = append(types, item.Get("type").String())
	}
	return types
}

// Erasing the tool_search round-trip froze the transcript one step before the
// search: the assistant announces it is about to load the tools and nothing
// records that it did, so the model reissues tool_search forever even though the
// loaded tools are already in the tools array. The pair has to survive as an
// ordinary function call plus its result.
func TestRewriteToolSearchInputItemsKeepsRoundTripAsFunctionCall(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"message","role":"user","content":"design something"},
			{"type":"tool_search_call","id":"fc_1","call_id":"call_1","status":"completed","arguments":{"query":"figma frame","limit":20}},
			{"type":"tool_search_output","id":"tso_1","call_id":"call_1","tools":[
				{"type":"namespace","name":"mcp__figma_mcp_go","tools":[
					{"type":"function","name":"create_frame"},
					{"type":"function","name":"create_section"}
				]}
			]}
		]
	}`)

	got := RewriteToolSearchInputItems(body)

	if types := inputItemTypes(t, got); len(types) != 3 ||
		types[0] != "message" || types[1] != "function_call" || types[2] != "function_call_output" {
		t.Fatalf("input types = %v, want [message function_call function_call_output]", types)
	}

	call := gjson.GetBytes(got, "input.1")
	if name := call.Get("name").String(); name != "tool_search" {
		t.Fatalf("function_call name = %q, want tool_search", name)
	}
	if callID := call.Get("call_id").String(); callID != "call_1" {
		t.Fatalf("function_call call_id = %q, want call_1", callID)
	}
	// Arguments must survive as a JSON string; the translator forwards them verbatim.
	if query := gjson.Get(call.Get("arguments").String(), "query").String(); query != "figma frame" {
		t.Fatalf("arguments = %q, want the original query", call.Get("arguments").String())
	}

	output := gjson.GetBytes(got, "input.2")
	if callID := output.Get("call_id").String(); callID != "call_1" {
		t.Fatalf("function_call_output call_id = %q, want call_1", callID)
	}
	// The names must match what the request translator forwards upstream, or the
	// model is told about tools under names it cannot call.
	text := output.Get("output").String()
	for _, want := range []string{"mcp__figma_mcp_go__create_frame", "mcp__figma_mcp_go__create_section"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output %q missing qualified name %q", text, want)
		}
	}
}

// Chat Completions upstreams reject an assistant tool_calls message whose
// tool_call_id has no matching tool message (Kimi: "the following tool_call_ids
// did not have response messages"), so a half of the pair on its own is dropped.
func TestRewriteToolSearchInputItemsDropsOrphans(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "output without call",
			body: `{"input":[
				{"type":"message","role":"user","content":"hi"},
				{"type":"tool_search_output","call_id":"call_1","tools":[{"type":"function","name":"js"}]}
			]}`,
		},
		{
			name: "call without output",
			body: `{"input":[
				{"type":"message","role":"user","content":"hi"},
				{"type":"tool_search_call","call_id":"call_1","arguments":{"query":"js"}}
			]}`,
		},
		{
			name: "missing call_id",
			body: `{"input":[
				{"type":"message","role":"user","content":"hi"},
				{"type":"tool_search_call","arguments":{"query":"js"}},
				{"type":"tool_search_output","tools":[{"type":"function","name":"js"}]}
			]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RewriteToolSearchInputItems([]byte(tc.body))
			types := inputItemTypes(t, got)
			if len(types) != 1 || types[0] != "message" {
				t.Fatalf("input types = %v, want [message]", types)
			}
		})
	}
}

// A search that matched nothing still has to advance the transcript, otherwise
// the model retries the same empty query.
func TestRewriteToolSearchInputItemsSummarizesEmptyResult(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"tool_search_call","call_id":"call_1","arguments":{"query":"nope"}},
		{"type":"tool_search_output","call_id":"call_1","tools":[]}
	]}`)

	got := RewriteToolSearchInputItems(body)
	output := gjson.GetBytes(got, "input.1.output").String()
	if output == "" {
		t.Fatalf("empty result produced no output text: %s", got)
	}
	if !strings.Contains(strings.ToLower(output), "no tools matched") {
		t.Fatalf("output = %q, want it to state that nothing matched", output)
	}
}

// Children that already carry the mcp__ prefix are forwarded unqualified, so the
// summary must not double-prefix them.
func TestRewriteToolSearchInputItemsDoesNotDoubleQualify(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"tool_search_call","call_id":"call_1","arguments":{"query":"js"}},
		{"type":"tool_search_output","call_id":"call_1","tools":[
			{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"mcp__node_repl__js"}]}
		]}
	]}`)

	got := RewriteToolSearchInputItems(body)
	output := gjson.GetBytes(got, "input.1.output").String()
	if strings.Contains(output, "codex_app__mcp__node_repl__js") {
		t.Fatalf("name double-qualified: %q", output)
	}
	if !strings.Contains(output, "mcp__node_repl__js") {
		t.Fatalf("output %q missing the tool name", output)
	}
}

// A payload with no tool_search items must come back byte-identical.
func TestRewriteToolSearchInputItemsLeavesUnrelatedInputAlone(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":"hi"}]}`)
	got := RewriteToolSearchInputItems(body)
	if string(got) != string(body) {
		t.Fatalf("body changed: %s", got)
	}
}
