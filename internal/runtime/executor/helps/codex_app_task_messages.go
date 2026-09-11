package helps

import (
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeCodexAppTaskMessages restores app-delivered task instructions to
// user messages. Codex records these deliveries as function_call_output items
// without call_id; they are not replies to model-issued tool calls. Keep the
// exact text and position without inventing a call or borrowing another ID.
func NormalizeCodexAppTaskMessages(payload []byte) []byte {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload
	}
	for index, item := range input.Array() {
		if item.Get("type").String() != "function_call_output" ||
			item.Get("namespace").String() != "codex_app" {
			continue
		}
		// Even an explicitly null or empty ID is outside this delivery shape.
		if _, exists := item.Map()["call_id"]; exists {
			continue
		}
		name := item.Get("name").String()
		if name != "create_thread" && name != "send_message_to_thread" {
			continue
		}
		output := item.Get("output")
		if output.Type != gjson.String {
			continue
		}
		message, errSet := sjson.SetBytes([]byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`), "content.0.text", output.String())
		if errSet != nil {
			return payload
		}
		updated, errSet := sjson.SetRawBytes(payload, "input."+strconv.Itoa(index), message)
		if errSet != nil {
			return payload
		}
		payload = updated
	}
	return payload
}
