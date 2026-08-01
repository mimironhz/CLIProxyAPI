package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestCoerceXAIToolArgumentsJSON(t *testing.T) {
	tests := []struct {
		name  string
		args  string
		want  string
		check func(t *testing.T, got string)
	}{
		{
			name: "known integer key loses the float form",
			args: `{"session_id":44906.0}`,
			want: `{"session_id":44906}`,
		},
		{
			name: "suffix-matched key is coerced",
			args: `{"yield_time_ms":250.0}`,
			want: `{"yield_time_ms":250}`,
		},
		{
			name: "non-whole values are left alone",
			args: `{"limit":2.5}`,
			want: `{"limit":2.5}`,
		},
		{
			name: "unrelated keys keep their float form",
			args: `{"temperature":0.7}`,
			want: `{"temperature":0.7}`,
		},
		{
			name: "already-integer literals are untouched",
			args: `{"limit":10}`,
			want: `{"limit":10}`,
		},
		{
			name: "nested objects and arrays are walked",
			args: `{"outer":{"pid":123.0},"items":[{"offset":5.0}]}`,
			check: func(t *testing.T, got string) {
				if v := gjson.Get(got, "outer.pid").Raw; v != "123" {
					t.Fatalf("outer.pid = %s, want 123; got=%s", v, got)
				}
				if v := gjson.Get(got, "items.0.offset").Raw; v != "5" {
					t.Fatalf("items.0.offset = %s, want 5; got=%s", v, got)
				}
			},
		},
		{
			name: "non-object payloads pass through",
			args: `not json`,
			want: `not json`,
		},
		{
			name: "large integers keep full precision",
			args: `{"count":9007199254740993}`,
			want: `{"count":9007199254740993}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := coerceXAIToolArgumentsJSON(test.args)
			if test.check != nil {
				test.check(t, got)
				return
			}
			if got != test.want {
				t.Fatalf("coerceXAIToolArgumentsJSON(%s) = %s, want %s", test.args, got, test.want)
			}
		})
	}
}

func TestRestoreXAIToolSearchCallsConvertsFunctionCall(t *testing.T) {
	event := []byte(`{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"tool_search","arguments":"{\"query\":\"node_repl js\",\"limit\":3.0}"}}`)

	got := restoreXAIToolSearchCalls(event)

	item := gjson.GetBytes(got, "item")
	if v := item.Get("type").String(); v != "tool_search_call" {
		t.Fatalf("item.type = %q, want tool_search_call; got=%s", v, string(got))
	}
	if v := item.Get("execution").String(); v != "client" {
		t.Fatalf("item.execution = %q, want client; got=%s", v, string(got))
	}
	if v := item.Get("status").String(); v != "completed" {
		t.Fatalf("item.status = %q, want completed; got=%s", v, string(got))
	}
	if v := item.Get("call_id").String(); v != "call_1" {
		t.Fatalf("item.call_id = %q, want call_1; got=%s", v, string(got))
	}
	// arguments must be an object, not the JSON string Grok emits.
	if !item.Get("arguments").IsObject() {
		t.Fatalf("item.arguments is not an object; got=%s", string(got))
	}
	if v := item.Get("arguments.query").String(); v != "node_repl js" {
		t.Fatalf("item.arguments.query = %q, want 'node_repl js'; got=%s", v, string(got))
	}
	if v := item.Get("arguments.limit").Raw; v != "3" {
		t.Fatalf("item.arguments.limit = %s, want 3; got=%s", v, string(got))
	}
}

func TestRestoreXAIToolSearchCallsMarksAddedInProgress(t *testing.T) {
	event := []byte(`{"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","name":"tool_search","arguments":""}}`)
	got := restoreXAIToolSearchCalls(event)
	if v := gjson.GetBytes(got, "item.status").String(); v != "in_progress" {
		t.Fatalf("item.status = %q, want in_progress; got=%s", v, string(got))
	}
	if v := gjson.GetBytes(got, "item.arguments").Raw; v != "{}" {
		t.Fatalf("item.arguments = %s, want {}; got=%s", v, string(got))
	}
}

func TestRestoreXAIToolSearchCallsRewritesCompletedOutput(t *testing.T) {
	event := []byte(`{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant"},{"type":"function_call","id":"fc_1","name":"tool_search","arguments":"{\"query\":\"js\"}"},{"type":"function_call","id":"fc_2","name":"shell","arguments":"{\"timeout_ms\":1000.0}"}]}}`)

	got := restoreXAIToolSearchCalls(event)

	if v := gjson.GetBytes(got, "response.output.1.type").String(); v != "tool_search_call" {
		t.Fatalf("output.1.type = %q, want tool_search_call; got=%s", v, string(got))
	}
	// Ordinary tool calls keep their shape but get integer-typed arguments.
	if v := gjson.GetBytes(got, "response.output.2.type").String(); v != "function_call" {
		t.Fatalf("output.2.type = %q, want function_call; got=%s", v, string(got))
	}
	arguments := gjson.GetBytes(got, "response.output.2.arguments").String()
	if v := gjson.Get(arguments, "timeout_ms").Raw; v != "1000" {
		t.Fatalf("output.2 timeout_ms = %s, want 1000; got=%s", v, string(got))
	}
}

func TestRestoreXAIToolSearchCallsLeavesUnrelatedEvents(t *testing.T) {
	event := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	if got := restoreXAIToolSearchCalls(event); string(got) != string(event) {
		t.Fatalf("event was rewritten:\n got=%s\nwant=%s", string(got), string(event))
	}
}
