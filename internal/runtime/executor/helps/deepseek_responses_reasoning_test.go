package helps

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAdaptDeepSeekResponsesReasoningEventRenamesTextEvents(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		wantType  string
		wantIndex int64
	}{
		{
			name:      "delta",
			data:      `{"type":"response.reasoning_text.delta","content_index":2,"item_id":"r1","output_index":0,"delta":"think"}`,
			wantType:  "response.reasoning_summary_text.delta",
			wantIndex: 2,
		},
		{
			name:      "done",
			data:      `{"type":"response.reasoning_text.done","content_index":0,"item_id":"r1","output_index":0,"text":"think"}`,
			wantType:  "response.reasoning_summary_text.done",
			wantIndex: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AdaptDeepSeekResponsesReasoningEvent([]byte(tt.data))
			if eventType := gjson.GetBytes(got, "type").String(); eventType != tt.wantType {
				t.Errorf("type = %q, want %q", eventType, tt.wantType)
			}
			if index := gjson.GetBytes(got, "summary_index").Int(); index != tt.wantIndex {
				t.Errorf("summary_index = %d, want %d", index, tt.wantIndex)
			}
			if gjson.GetBytes(got, "content_index").Exists() {
				t.Errorf("content_index survived the rename: %s", got)
			}
		})
	}
}

func TestAdaptDeepSeekResponsesReasoningEventRenamesReasoningParts(t *testing.T) {
	data := []byte(`{"type":"response.content_part.done","content_index":0,"item_id":"r1","output_index":0,"part":{"type":"reasoning_text","text":"think"}}`)
	got := AdaptDeepSeekResponsesReasoningEvent(data)

	if eventType := gjson.GetBytes(got, "type").String(); eventType != "response.reasoning_summary_part.done" {
		t.Errorf("type = %q, want response.reasoning_summary_part.done", eventType)
	}
	if partType := gjson.GetBytes(got, "part.type").String(); partType != "summary_text" {
		t.Errorf("part.type = %q, want summary_text", partType)
	}
	if text := gjson.GetBytes(got, "part.text").String(); text != "think" {
		t.Errorf("part.text = %q, want think", text)
	}
}

// The same event type also carries the assistant's visible output, which must
// not be relabelled as reasoning.
func TestAdaptDeepSeekResponsesReasoningEventLeavesOutputTextParts(t *testing.T) {
	data := []byte(`{"type":"response.content_part.done","content_index":0,"item_id":"m1","output_index":1,"part":{"type":"output_text","text":"answer"}}`)
	got := AdaptDeepSeekResponsesReasoningEvent(data)

	if eventType := gjson.GetBytes(got, "type").String(); eventType != "response.content_part.done" {
		t.Errorf("type = %q, want response.content_part.done", eventType)
	}
	if index := gjson.GetBytes(got, "content_index"); !index.Exists() {
		t.Errorf("content_index was dropped from an output_text part: %s", got)
	}
}

func TestAdaptDeepSeekResponsesReasoningEventMirrorsItemContent(t *testing.T) {
	data := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"r1","content":[{"type":"reasoning_text","text":"first"},{"type":"reasoning_text","text":"second"}],"summary":[]}}`)
	got := AdaptDeepSeekResponsesReasoningEvent(data)

	summary := gjson.GetBytes(got, "item.summary")
	if !summary.IsArray() || len(summary.Array()) != 1 {
		t.Fatalf("item.summary = %s, want a single part", summary.Raw)
	}
	if partType := summary.Array()[0].Get("type").String(); partType != "summary_text" {
		t.Errorf("summary part type = %q, want summary_text", partType)
	}
	if text := summary.Array()[0].Get("text").String(); text != "first\nsecond" {
		t.Errorf("summary text = %q, want the joined content parts", text)
	}
	if content := gjson.GetBytes(got, "item.content"); len(content.Array()) != 2 {
		t.Errorf("original content was disturbed: %s", content.Raw)
	}
}

// A provider that does report a real summary must keep it.
func TestAdaptDeepSeekResponsesReasoningEventKeepsExistingSummary(t *testing.T) {
	data := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"r1","content":[{"type":"reasoning_text","text":"raw"}],"summary":[{"type":"summary_text","text":"kept"}]}}`)
	got := AdaptDeepSeekResponsesReasoningEvent(data)

	if text := gjson.GetBytes(got, "item.summary.0.text").String(); text != "kept" {
		t.Errorf("summary text = %q, want kept", text)
	}
}

// An in-progress item has no content yet; the matching done event supplies it.
func TestAdaptDeepSeekResponsesReasoningEventIgnoresEmptyItem(t *testing.T) {
	data := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"r1","content":[],"summary":[]}}`)
	got := AdaptDeepSeekResponsesReasoningEvent(data)

	if summary := gjson.GetBytes(got, "item.summary"); len(summary.Array()) != 0 {
		t.Errorf("item.summary = %s, want it left empty", summary.Raw)
	}
}

func TestAdaptDeepSeekResponsesReasoningMirrorsNonStreamOutput(t *testing.T) {
	body := []byte(`{"output":[{"type":"reasoning","id":"r1","content":[{"type":"reasoning_text","text":"think"}],"summary":[]},{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`)
	got := AdaptDeepSeekResponsesReasoning(body)

	if text := gjson.GetBytes(got, "output.0.summary.0.text").String(); text != "think" {
		t.Errorf("reasoning summary = %q, want think", text)
	}
	if text := gjson.GetBytes(got, "output.1.content.0.text").String(); text != "answer" {
		t.Errorf("message content was disturbed: %s", got)
	}
}

func TestRestoreDeepSeekResponsesReasoningContent(t *testing.T) {
	sealed := deepSeekReasoningPrefix + base64.StdEncoding.EncodeToString([]byte("sealed thought"))
	body := []byte(`{"input":[` +
		`{"type":"reasoning","id":"r1","summary":[],"encrypted_content":"` + sealed + `"},` +
		`{"type":"reasoning","id":"r2","summary":[{"type":"summary_text","text":"from summary"}]},` +
		`{"type":"reasoning","id":"r3","content":[{"type":"reasoning_text","text":"already here"}]},` +
		`{"type":"reasoning","id":"r4","summary":[],"encrypted_content":"gAAAAAB-some-other-provider"},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}` +
		`]}`)

	got := RestoreDeepSeekResponsesReasoningContent(body)

	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "sealed thought" {
		t.Errorf("unsealed content = %q, want sealed thought", text)
	}
	if partType := gjson.GetBytes(got, "input.0.content.0.type").String(); partType != "reasoning_text" {
		t.Errorf("restored part type = %q, want reasoning_text", partType)
	}
	if text := gjson.GetBytes(got, "input.1.content.0.text").String(); text != "from summary" {
		t.Errorf("content from summary = %q, want from summary", text)
	}
	if content := gjson.GetBytes(got, "input.2.content"); len(content.Array()) != 1 || content.Array()[0].Get("text").String() != "already here" {
		t.Errorf("existing content was rewritten: %s", content.Raw)
	}
	if gjson.GetBytes(got, "input.3.content").Exists() {
		t.Errorf("a foreign encrypted blob was decoded as text: %s", got)
	}
	if gjson.GetBytes(got, "input.4.content.0.type").String() != "input_text" {
		t.Errorf("a user message was rewritten: %s", got)
	}
}

func TestResponsesStreamChunkNamesItsOwnEvent(t *testing.T) {
	got := ResponsesStreamChunk([]byte(`{"type":"response.completed","response":{}}`))
	want := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}"
	if string(got) != want {
		t.Errorf("chunk = %q, want %q", got, want)
	}
}

func TestIsResponsesTerminalEvent(t *testing.T) {
	terminal := []string{"response.completed", "response.incomplete", "response.failed"}
	for _, eventType := range terminal {
		if !IsResponsesTerminalEvent([]byte(`{"type":"` + eventType + `"}`)) {
			t.Errorf("%s was not treated as terminal", eventType)
		}
	}
	if IsResponsesTerminalEvent([]byte(`{"type":"response.output_text.delta"}`)) {
		t.Error("a delta event was treated as terminal")
	}
}

func TestResponsesStreamFailure(t *testing.T) {
	message, failed := ResponsesStreamFailure([]byte(`{"type":"response.failed","response":{"error":{"message":"upstream exploded"}}}`))
	if !failed {
		t.Fatal("response.failed was not reported as a failure")
	}
	if message != "upstream exploded" {
		t.Errorf("message = %q, want upstream exploded", message)
	}
	if _, failed = ResponsesStreamFailure([]byte(`{"type":"response.completed"}`)); failed {
		t.Error("response.completed was reported as a failure")
	}
}

func TestResponsesStreamEventData(t *testing.T) {
	if data, ok := ResponsesStreamEventData([]byte(`data: {"type":"response.created"}`)); !ok || !strings.Contains(string(data), "response.created") {
		t.Errorf("data line was not extracted: %q %v", data, ok)
	}
	for _, line := range []string{"event: response.created", ": comment", "data: [DONE]", ""} {
		if _, ok := ResponsesStreamEventData([]byte(line)); ok {
			t.Errorf("%q was treated as an event payload", line)
		}
	}
}
