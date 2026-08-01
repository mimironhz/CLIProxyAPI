package helps

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// deepSeekReasoningPrefix tags the reasoning state this proxy mints so it can be
// told apart from a real provider blob on the way back in. The payload after the
// prefix is base64 of the plain summary text, mirroring the existing compaction
// state in kimi_executor.go — it is not encrypted, despite the field name, so
// treat any log carrying it as holding the model's reasoning in clear.
const deepSeekReasoningPrefix = "deepseek-reasoning-v1:"

// IsDeepSeekBaseURL reports whether an openai-compatibility base URL points at
// DeepSeek's own API. The check is on the host rather than the configured
// provider name, which is arbitrary user text.
func IsDeepSeekBaseURL(baseURL string) bool {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return false
	}
	parsed, errParse := url.Parse(trimmed)
	if errParse != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "deepseek.com" || strings.HasSuffix(host, ".deepseek.com")
}

// EnsureDeepSeekReasoningContent gives every assistant message that carries
// tool_calls a reasoning_content field, which DeepSeek's thinking models
// require: without it the request is rejected with "The `reasoning_content` in
// the thinking mode must be passed back to the API." Assistant messages that
// only carry text are exempt, and the requirement applies whether or not
// reasoning_effort is set.
//
// The Responses-to-Chat-Completions translation drops reasoning items, so the
// text is recovered from the pre-translation payload where the client sent one.
// When it did not — Hermes, for instance, replays function_call items with no
// reasoning item at all — the field is set to an empty string, which DeepSeek
// accepts. Nothing is invented: echoing the assistant's visible text back as its
// private reasoning would fabricate a chain of thought the model never had.
func EnsureDeepSeekReasoningContent(translated, responsesPayload []byte) []byte {
	messages := gjson.GetBytes(translated, "messages")
	if !messages.IsArray() {
		return translated
	}
	byCallID := deepSeekReasoningByCallID(responsesPayload)

	updated := translated
	for index, message := range messages.Array() {
		if strings.TrimSpace(message.Get("role").String()) != "assistant" {
			continue
		}
		toolCalls := message.Get("tool_calls")
		if !toolCalls.IsArray() || len(toolCalls.Array()) == 0 {
			continue
		}
		if message.Get("reasoning_content").Exists() {
			continue
		}
		reasoning := ""
		for _, toolCall := range toolCalls.Array() {
			id := strings.TrimSpace(toolCall.Get("id").String())
			if text, ok := byCallID[id]; ok && text != "" {
				reasoning = text
				break
			}
		}
		next, errSet := sjson.SetBytes(updated, "messages."+strconv.Itoa(index)+".reasoning_content", reasoning)
		if errSet != nil {
			continue
		}
		updated = next
	}
	return updated
}

// deepSeekReasoningByCallID maps each function_call's call_id to the reasoning
// text that preceded it in the Responses input. A reasoning item can cover
// several following calls, so it stays current until a non-call item ends the
// run.
func deepSeekReasoningByCallID(responsesPayload []byte) map[string]string {
	byCallID := make(map[string]string)
	input := gjson.GetBytes(responsesPayload, "input")
	if !input.IsArray() {
		return byCallID
	}
	current := ""
	for _, item := range input.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "reasoning":
			current = reasoningItemText(item)
		case "function_call":
			if id := strings.TrimSpace(item.Get("call_id").String()); id != "" && current != "" {
				byCallID[id] = current
			}
		case "function_call_output":
			// Keeps the run open: the output belongs to the call just recorded.
		default:
			current = ""
		}
	}
	return byCallID
}

// SealDeepSeekReasoning gives every reasoning item in a non-stream Responses
// body an encrypted_content blob carrying its own summary.
//
// DeepSeek returns reasoning as plain text and the translator emits it as an
// empty encrypted_content with a populated summary. Clients modelled on OpenAI
// semantics treat encrypted_content as the item itself and discard anything
// without one — Hermes does exactly this in
// agent/codex_responses_adapter.py, dropping the item and its summary — so the
// model's own reasoning never comes back and it re-derives its approach on every
// tool call. Sealing the summary into the field they do read makes them replay
// it; the request translator then maps that summary onto reasoning_content.
func SealDeepSeekReasoning(body []byte) []byte {
	output := gjson.GetBytes(body, "output")
	if !output.IsArray() {
		return body
	}
	for index := range output.Array() {
		body = sealDeepSeekReasoningAtPath(body, "output."+strconv.Itoa(index))
	}
	return body
}

// SealDeepSeekReasoningStreamChunk applies the same sealing to a translated SSE
// chunk. Hermes streams, so this is the path that actually matters.
func SealDeepSeekReasoningStreamChunk(chunk []byte) []byte {
	if len(chunk) == 0 || !bytes.Contains(chunk, []byte("reasoning")) {
		return chunk
	}
	lines := bytes.Split(chunk, []byte("\n"))
	changed := false
	for i, line := range lines {
		trimmed := bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(trimmed, []byte(sseDataTag)) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[len(sseDataTag):])
		sealed := sealDeepSeekReasoningEvent(payload)
		if bytes.Equal(sealed, payload) {
			continue
		}
		lines[i] = append([]byte(sseDataTag), sealed...)
		changed = true
	}
	if !changed {
		return chunk
	}
	return bytes.Join(lines, []byte("\n"))
}

func sealDeepSeekReasoningEvent(data []byte) []byte {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return data
	}
	switch gjson.GetBytes(data, "type").String() {
	case "response.output_item.added", "response.output_item.done":
		return sealDeepSeekReasoningAtPath(data, "item")
	case "response.completed":
		output := gjson.GetBytes(data, "response.output")
		if !output.IsArray() {
			return data
		}
		for index := range output.Array() {
			data = sealDeepSeekReasoningAtPath(data, fmt.Sprintf("response.output.%d", index))
		}
		return data
	}
	return data
}

// sealDeepSeekReasoningAtPath seals one item, leaving every other item and any
// blob the upstream genuinely issued untouched.
func sealDeepSeekReasoningAtPath(data []byte, path string) []byte {
	item := gjson.GetBytes(data, path)
	if !item.IsObject() || item.Get("type").String() != "reasoning" {
		return data
	}
	if strings.TrimSpace(item.Get("encrypted_content").String()) != "" {
		return data
	}
	text := reasoningItemText(item)
	if text == "" {
		// An in-progress item has no summary yet; the matching done event does.
		return data
	}
	updated, errSet := sjson.SetBytes(data, path+".encrypted_content",
		deepSeekReasoningPrefix+base64.StdEncoding.EncodeToString([]byte(text)))
	if errSet != nil {
		return data
	}
	return updated
}

// unsealDeepSeekReasoning recovers text this proxy sealed. A blob minted
// elsewhere returns empty rather than garbage.
func unsealDeepSeekReasoning(encryptedContent string) string {
	if !strings.HasPrefix(encryptedContent, deepSeekReasoningPrefix) {
		return ""
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(strings.TrimPrefix(encryptedContent, deepSeekReasoningPrefix))
	if errDecode != nil {
		return ""
	}
	return strings.TrimSpace(string(decoded))
}

// reasoningItemText prefers the summary Codex renders, falls back to the item's
// content parts, and finally to state this proxy sealed itself. A blob from any
// other issuer is ignored: it is not readable text and DeepSeek cannot consume
// another provider's state.
func reasoningItemText(item gjson.Result) string {
	for _, field := range []string{"summary", "content"} {
		parts := item.Get(field)
		if !parts.IsArray() {
			continue
		}
		texts := make([]string, 0, len(parts.Array()))
		for _, part := range parts.Array() {
			if text := strings.TrimSpace(part.Get("text").String()); text != "" {
				texts = append(texts, text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return unsealDeepSeekReasoning(item.Get("encrypted_content").String())
}
