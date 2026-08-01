package helps

import (
	"fmt"
	"strings"
	"testing"
)

func TestCompactionDiagnosticFieldsResponsesPayload(t *testing.T) {
	payload := []byte(`{
		"model":"grok-4.5",
		"prompt_cache_key":"thread-1",
		"client_metadata":{
			"session_id":"session-fallback",
			"x-codex-turn-metadata":"{\"thread_id\":\"thread-1\",\"turn_id\":\"turn-1\",\"window_id\":\"thread-1:2\",\"session_id\":\"session-1\"}"
		},
		"input":[
			{"type":"message","role":"user","content":"continue"},
			{"type":"compaction","id":"cmp-in","encrypted_content":"foreign-state"},
			{"type":"compaction_trigger"}
		],
		"output":[
			{"type":"message","id":"msg-retained"},
			{"type":"compaction","id":"cmp-out","encrypted_content":"new-state"}
		],
		"usage":{"input_tokens":120,"output_tokens":30,"total_tokens":150}
	}`)

	fields := CompactionDiagnosticFields(payload)

	assertCompactionDiagnosticField(t, fields, "model", "grok-4.5")
	assertCompactionDiagnosticField(t, fields, "thread_id", "thread-1")
	assertCompactionDiagnosticField(t, fields, "turn_id", "turn-1")
	assertCompactionDiagnosticField(t, fields, "window_id", "thread-1:2")
	assertCompactionDiagnosticField(t, fields, "session_id", "session-1")
	assertCompactionDiagnosticField(t, fields, "input_items", 3)
	assertCompactionDiagnosticField(t, fields, "input_types", "compaction=1,compaction_trigger=1,message=1")
	assertCompactionDiagnosticField(t, fields, "output_items", 2)
	assertCompactionDiagnosticField(t, fields, "output_types", "compaction=1,message=1")
	assertCompactionDiagnosticField(t, fields, "compaction_items", 2)
	assertCompactionDiagnosticField(t, fields, "usage_input_tokens", int64(120))
	assertCompactionDiagnosticField(t, fields, "usage_output_tokens", int64(30))
	assertCompactionDiagnosticField(t, fields, "usage_total_tokens", int64(150))

	refs, ok := fields["compaction_refs"].(string)
	if !ok || !strings.Contains(refs, "id=cmp-in,chars=13,sha256=") || !strings.Contains(refs, "id=cmp-out,chars=9,sha256=") {
		t.Fatalf("compaction_refs = %#v", fields["compaction_refs"])
	}
	if got, ok := fields["payload_sha256"].(string); !ok || len(got) != 64 {
		t.Fatalf("payload_sha256 = %#v, want 64 hex characters", fields["payload_sha256"])
	}
}

func TestCompactionDiagnosticFieldsChatPayload(t *testing.T) {
	payload := []byte(`{
		"id":"chatcmpl-1",
		"object":"chat.completion",
		"messages":[{"role":"system"},{"role":"user"},{"role":"user"}],
		"choices":[{"finish_reason":"stop","message":{"content":"  compact summary  "}}],
		"usage":{"prompt_tokens":200,"completion_tokens":25,"total_tokens":225}
	}`)

	fields := CompactionDiagnosticFields(payload)

	assertCompactionDiagnosticField(t, fields, "response_id", "chatcmpl-1")
	assertCompactionDiagnosticField(t, fields, "messages", 3)
	assertCompactionDiagnosticField(t, fields, "message_roles", "system=1,user=2")
	assertCompactionDiagnosticField(t, fields, "choices", 1)
	assertCompactionDiagnosticField(t, fields, "finish_reason", "stop")
	assertCompactionDiagnosticField(t, fields, "summary_chars", 15)
	assertCompactionDiagnosticField(t, fields, "usage_prompt_tokens", int64(200))
	assertCompactionDiagnosticField(t, fields, "usage_completion_tokens", int64(25))
}

func TestCompactionDiagnosticFieldsInvalidPayload(t *testing.T) {
	fields := CompactionDiagnosticFields([]byte("not-json"))
	assertCompactionDiagnosticField(t, fields, "payload_bytes", 8)
	assertCompactionDiagnosticField(t, fields, "payload_json", false)
	if _, ok := fields["input_items"]; ok {
		t.Fatalf("invalid payload unexpectedly produced input_items: %#v", fields)
	}
}

func TestCompactionDiagnosticFieldsDoNotExposeContent(t *testing.T) {
	const canary = "super-secret-conversation-canary"
	payload := []byte(`{"model":"kimi-k3","input":[{"type":"message","role":"user","content":"` + canary + `"},{"type":"compaction","id":"cmp-1","encrypted_content":"` + canary + `"}]}`)

	fields := CompactionDiagnosticFields(payload)

	if rendered := fmt.Sprint(fields); strings.Contains(rendered, canary) {
		t.Fatalf("diagnostic fields leaked payload content: %s", rendered)
	}
}

func TestCompactionStreamDiagnosticFields(t *testing.T) {
	chunks := [][]byte{
		[]byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"),
		[]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\"}\n\n"),
		[]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"compaction\",\"id\":\"cmp-1\",\"encrypted_content\":\"state\"}],\"usage\":{\"total_tokens\":42}}}\n\n"),
	}

	fields := CompactionStreamDiagnosticFields(chunks)

	assertCompactionDiagnosticField(t, fields, "stream_chunks", 3)
	assertCompactionDiagnosticField(t, fields, "stream_events", "response.completed=1,response.created=1,response.output_item.done=1")
	assertCompactionDiagnosticField(t, fields, "response_id", "resp-1")
	assertCompactionDiagnosticField(t, fields, "status", "completed")
	assertCompactionDiagnosticField(t, fields, "output_items", 1)
	assertCompactionDiagnosticField(t, fields, "output_types", "compaction=1")
	assertCompactionDiagnosticField(t, fields, "usage_total_tokens", int64(42))
	if got, ok := fields["stream_sha256"].(string); !ok || len(got) != 64 {
		t.Fatalf("stream_sha256 = %#v, want 64 hex characters", fields["stream_sha256"])
	}
}

func assertCompactionDiagnosticField(t *testing.T, fields map[string]any, key string, want any) {
	t.Helper()
	if got := fields[key]; got != want {
		t.Fatalf("%s = %#v, want %#v; fields=%#v", key, got, want, fields)
	}
}
