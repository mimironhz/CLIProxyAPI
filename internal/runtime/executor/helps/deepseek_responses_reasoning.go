package helps

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// DeepSeek's Responses API reports the chain of thought as the reasoning item's
// own content — content[].reasoning_text, streamed as response.reasoning_text.*
// — and always leaves summary empty. OpenAI instead reports a summary, streamed
// as response.reasoning_summary_*, and Codex renders the summary while treating
// content as raw reasoning it hides unless explicitly asked for.
//
// The Chat Completions path never had to care: its translator mapped DeepSeek's
// reasoning_content onto summary on the way out. Forwarding the native Responses
// stream unchanged would therefore silently stop Codex Desktop from showing any
// reasoning at all, so the events and items are renamed here to the summary form
// the clients already consume. The original content is left in place, because
// DeepSeek does accept a reasoning item's content back as input and replaying it
// gives the model its own chain of thought across a tool call.
//
// Sealing into encrypted_content still happens afterwards through
// SealDeepSeekReasoning / SealDeepSeekReasoningStreamChunk, unchanged.

// AdaptDeepSeekResponsesReasoning mirrors the reasoning content of every item in
// a non-stream Responses body into that item's summary.
func AdaptDeepSeekResponsesReasoning(body []byte) []byte {
	output := gjson.GetBytes(body, "output")
	if !output.IsArray() {
		return body
	}
	for index := range output.Array() {
		body = mirrorDeepSeekReasoningSummary(body, "output."+strconv.Itoa(index))
	}
	return body
}

// AdaptDeepSeekResponsesReasoningEvent rewrites one Responses SSE data payload so
// the reasoning it carries reaches the client as a summary. Events that carry no
// reasoning are returned unchanged.
func AdaptDeepSeekResponsesReasoningEvent(data []byte) []byte {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return data
	}
	switch gjson.GetBytes(data, "type").String() {
	case "response.reasoning_text.delta":
		return renameDeepSeekReasoningTextEvent(data, "response.reasoning_summary_text.delta")
	case "response.reasoning_text.done":
		return renameDeepSeekReasoningTextEvent(data, "response.reasoning_summary_text.done")
	case "response.content_part.added":
		return renameDeepSeekReasoningPartEvent(data, "response.reasoning_summary_part.added")
	case "response.content_part.done":
		return renameDeepSeekReasoningPartEvent(data, "response.reasoning_summary_part.done")
	case "response.output_item.added", "response.output_item.done":
		return mirrorDeepSeekReasoningSummary(data, "item")
	case "response.completed", "response.incomplete":
		output := gjson.GetBytes(data, "response.output")
		if !output.IsArray() {
			return data
		}
		for index := range output.Array() {
			data = mirrorDeepSeekReasoningSummary(data, "response.output."+strconv.Itoa(index))
		}
		return data
	}
	return data
}

// renameDeepSeekReasoningTextEvent relabels a raw reasoning delta or done event
// as its summary counterpart. Summary events are indexed by summary_index rather
// than content_index, so the index moves with the rename.
func renameDeepSeekReasoningTextEvent(data []byte, eventType string) []byte {
	updated, errSet := sjson.SetBytes(data, "type", eventType)
	if errSet != nil {
		return data
	}
	return moveDeepSeekContentIndex(updated)
}

// renameDeepSeekReasoningPartEvent relabels a content part event only when the
// part is reasoning text; the same event type also carries assistant output_text
// parts, which must pass through untouched.
func renameDeepSeekReasoningPartEvent(data []byte, eventType string) []byte {
	part := gjson.GetBytes(data, "part")
	if !part.IsObject() || part.Get("type").String() != "reasoning_text" {
		return data
	}
	updated, errSet := sjson.SetBytes(data, "type", eventType)
	if errSet != nil {
		return data
	}
	if updated, errSet = sjson.SetBytes(updated, "part", map[string]any{
		"type": "summary_text",
		"text": part.Get("text").String(),
	}); errSet != nil {
		return data
	}
	return moveDeepSeekContentIndex(updated)
}

func moveDeepSeekContentIndex(data []byte) []byte {
	contentIndex := gjson.GetBytes(data, "content_index")
	if !contentIndex.Exists() {
		return data
	}
	updated, errSet := sjson.SetBytes(data, "summary_index", contentIndex.Int())
	if errSet != nil {
		return data
	}
	if pruned, errDelete := sjson.DeleteBytes(updated, "content_index"); errDelete == nil {
		updated = pruned
	}
	return updated
}

// mirrorDeepSeekReasoningSummary copies a reasoning item's content parts into its
// summary. An item that already carries a summary is left alone so a genuine
// upstream summary is never overwritten.
func mirrorDeepSeekReasoningSummary(data []byte, path string) []byte {
	item := gjson.GetBytes(data, path)
	if !item.IsObject() || item.Get("type").String() != "reasoning" {
		return data
	}
	if summary := item.Get("summary"); summary.IsArray() && len(summary.Array()) > 0 {
		return data
	}
	text := deepSeekReasoningContentText(item)
	if text == "" {
		// An in-progress item has no content yet; the matching done event does.
		return data
	}
	updated, errSet := sjson.SetBytes(data, path+".summary", []map[string]any{{
		"type": "summary_text",
		"text": text,
	}})
	if errSet != nil {
		return data
	}
	return updated
}

func deepSeekReasoningContentText(item gjson.Result) string {
	parts := item.Get("content")
	if !parts.IsArray() {
		return ""
	}
	texts := make([]string, 0, len(parts.Array()))
	for _, part := range parts.Array() {
		if text := strings.TrimSpace(part.Get("text").String()); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// ResponsesStreamChunk renders one Responses event as the two-line SSE chunk the
// Responses handlers and Codex clients expect. The event name is taken from the
// payload so a renamed event can never disagree with its own type field.
func ResponsesStreamChunk(data []byte) []byte {
	eventType := gjson.GetBytes(data, "type").String()
	if eventType == "" {
		return append([]byte("data: "), data...)
	}
	chunk := make([]byte, 0, len(eventType)+len(data)+14)
	chunk = append(chunk, "event: "...)
	chunk = append(chunk, eventType...)
	chunk = append(chunk, '\n')
	chunk = append(chunk, "data: "...)
	chunk = append(chunk, data...)
	return chunk
}

// IsResponsesTerminalEvent reports whether an event ends the stream. A Responses
// stream closes with response.completed, response.incomplete or response.failed;
// DeepSeek, unlike Chat Completions upstreams, sends no data: [DONE] marker
// afterwards, so the terminal event is the only signal that the turn is over.
func IsResponsesTerminalEvent(data []byte) bool {
	switch gjson.GetBytes(data, "type").String() {
	case "response.completed", "response.incomplete", "response.failed":
		return true
	}
	return false
}

// ResponsesStreamFailure reports the error carried by a response.failed event.
// A failed turn arrives inside a 200 stream, so without this it would reach the
// client as a silently truncated response.
func ResponsesStreamFailure(data []byte) (string, bool) {
	if gjson.GetBytes(data, "type").String() != "response.failed" {
		return "", false
	}
	errorNode := gjson.GetBytes(data, "response.error")
	if !errorNode.Exists() {
		errorNode = gjson.GetBytes(data, "error")
	}
	if message := strings.TrimSpace(errorNode.Get("message").String()); message != "" {
		return message, true
	}
	if errorNode.Exists() && errorNode.Raw != "" && errorNode.Raw != "null" {
		return errorNode.Raw, true
	}
	return "upstream reported response.failed", true
}

// RestoreDeepSeekResponsesReasoningContent rebuilds the content of reasoning
// items that a client replayed without one.
//
// DeepSeek reads a reasoning item's plain-text content and ignores both summary
// and encrypted_content on input, while Codex-shaped clients round-trip the
// summary and the sealed blob. Left alone, every replayed turn would therefore
// arrive stripped of the model's own chain of thought — a regression against the
// Chat Completions path, which passed it back as reasoning_content. Items that
// already carry content, and blobs minted by another provider, are untouched.
func RestoreDeepSeekResponsesReasoningContent(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	for index, item := range input.Array() {
		if !item.IsObject() || item.Get("type").String() != "reasoning" {
			continue
		}
		if content := item.Get("content"); content.IsArray() && len(content.Array()) > 0 {
			continue
		}
		text := reasoningItemText(item)
		if text == "" {
			continue
		}
		updated, errSet := sjson.SetBytes(body, "input."+strconv.Itoa(index)+".content", []map[string]any{{
			"type": "reasoning_text",
			"text": text,
		}})
		if errSet != nil {
			continue
		}
		body = updated
	}
	return body
}

// ResponsesStreamEventData extracts the JSON payload of an SSE data line,
// returning false for comments, event names and the [DONE] marker.
func ResponsesStreamEventData(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte(sseDataTag)) {
		return nil, false
	}
	data := bytes.TrimSpace(trimmed[len(sseDataTag):])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return nil, false
	}
	return data, true
}
