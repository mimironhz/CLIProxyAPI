package helps

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const maxCompactionDiagnosticRefs = 32

// CompactionDiagnosticFields returns secret-safe structural metadata for a
// compaction request or response. Raw payloads stay in the dedicated request
// log; main.log gets stable fingerprints and enough identity to correlate a
// failed compaction even when the downstream websocket has not closed yet.
func CompactionDiagnosticFields(payload []byte) log.Fields {
	fields := log.Fields{
		"payload_bytes":  len(payload),
		"payload_sha256": compactionDiagnosticSHA256(payload),
		"payload_json":   gjson.ValidBytes(payload),
	}
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return fields
	}

	root := gjson.ParseBytes(payload)
	setCompactionDiagnosticString(fields, "model", root.Get("model").String())
	setCompactionDiagnosticString(fields, "object", root.Get("object").String())
	setCompactionDiagnosticString(fields, "response_id", root.Get("id").String())
	setCompactionDiagnosticString(fields, "status", root.Get("status").String())
	setCompactionDiagnosticString(fields, "prompt_cache_key", root.Get("prompt_cache_key").String())

	setCompactionDiagnosticIdentity(fields, root)
	setCompactionDiagnosticItems(fields, "input", root.Get("input"))
	setCompactionDiagnosticItems(fields, "output", root.Get("output"))
	setCompactionDiagnosticMessages(fields, root.Get("messages"))
	setCompactionDiagnosticUsage(fields, root.Get("usage"))
	setCompactionDiagnosticChoices(fields, root.Get("choices"))

	refs := append(compactionDiagnosticRefs(root.Get("input")), compactionDiagnosticRefs(root.Get("output"))...)
	if len(refs) > 0 {
		fields["compaction_items"] = len(refs)
		if len(refs) > maxCompactionDiagnosticRefs {
			fields["compaction_refs"] = strings.Join(refs[:maxCompactionDiagnosticRefs], ";") + fmt.Sprintf(";...+%d", len(refs)-maxCompactionDiagnosticRefs)
		} else {
			fields["compaction_refs"] = strings.Join(refs, ";")
		}
	}
	return fields
}

// CompactionStreamDiagnosticFields summarizes the exact synthesized SSE stream
// returned to a downstream client and, when present, the completed response it
// carries. It intentionally records no event body text.
func CompactionStreamDiagnosticFields(chunks [][]byte) log.Fields {
	joined := bytes.Join(chunks, nil)
	fields := log.Fields{
		"stream_bytes":  len(joined),
		"stream_chunks": len(chunks),
		"stream_sha256": compactionDiagnosticSHA256(joined),
	}

	eventCounts := make(map[string]int)
	for _, chunk := range chunks {
		for _, line := range bytes.Split(chunk, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			switch {
			case bytes.HasPrefix(line, []byte("event:")):
				eventType := strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
				if eventType != "" {
					eventCounts[eventType]++
				}
			case bytes.HasPrefix(line, []byte("data:")):
				payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
				if gjson.GetBytes(payload, "type").String() != "response.completed" {
					continue
				}
				response := gjson.GetBytes(payload, "response")
				if !response.Exists() || response.Type != gjson.JSON {
					continue
				}
				for key, value := range CompactionDiagnosticFields([]byte(response.Raw)) {
					fields[key] = value
				}
			}
		}
	}
	if len(eventCounts) > 0 {
		keys := make([]string, 0, len(eventCounts))
		for key := range eventCounts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", key, eventCounts[key]))
		}
		fields["stream_events"] = strings.Join(parts, ",")
	}
	return fields
}

func setCompactionDiagnosticIdentity(fields log.Fields, root gjson.Result) {
	turnMetadata := root.Get("client_metadata.x-codex-turn-metadata").String()
	metadata := gjson.Parse(turnMetadata)
	for _, identity := range []struct {
		field string
		path  string
	}{
		{field: "thread_id", path: "thread_id"},
		{field: "turn_id", path: "turn_id"},
		{field: "window_id", path: "window_id"},
		{field: "session_id", path: "session_id"},
	} {
		value := metadata.Get(identity.path).String()
		if value == "" {
			value = root.Get("client_metadata." + identity.path).String()
		}
		setCompactionDiagnosticString(fields, identity.field, value)
	}

	if _, ok := fields["session_id"]; !ok {
		userID := root.Get("metadata.user_id").String()
		setCompactionDiagnosticString(fields, "session_id", gjson.Get(userID, "session_id").String())
	}
	if _, ok := fields["turn_id"]; !ok {
		for _, item := range root.Get("input").Array() {
			turnID := item.Get("internal_chat_message_metadata_passthrough.turn_id").String()
			if turnID != "" {
				fields["turn_id"] = turnID
			}
		}
	}
}

func setCompactionDiagnosticItems(fields log.Fields, prefix string, items gjson.Result) {
	if !items.IsArray() {
		return
	}
	array := items.Array()
	fields[prefix+"_items"] = len(array)
	fields[prefix+"_types"] = compactionDiagnosticTypeCounts(array, "type")
}

func setCompactionDiagnosticMessages(fields log.Fields, messages gjson.Result) {
	if !messages.IsArray() {
		return
	}
	array := messages.Array()
	fields["messages"] = len(array)
	fields["message_roles"] = compactionDiagnosticTypeCounts(array, "role")
}

func setCompactionDiagnosticUsage(fields log.Fields, usage gjson.Result) {
	if !usage.Exists() || usage.Type != gjson.JSON {
		return
	}
	for _, tokenField := range []string{"input_tokens", "prompt_tokens", "output_tokens", "completion_tokens", "total_tokens"} {
		value := usage.Get(tokenField)
		if value.Exists() {
			fields["usage_"+tokenField] = value.Int()
		}
	}
}

func setCompactionDiagnosticChoices(fields log.Fields, choices gjson.Result) {
	if !choices.IsArray() {
		return
	}
	array := choices.Array()
	fields["choices"] = len(array)
	if len(array) == 0 {
		return
	}
	setCompactionDiagnosticString(fields, "finish_reason", array[0].Get("finish_reason").String())
	summary := strings.TrimSpace(array[0].Get("message.content").String())
	if summary != "" {
		fields["summary_chars"] = len(summary)
		fields["summary_sha256"] = compactionDiagnosticSHA256([]byte(summary))
	}
}

func compactionDiagnosticTypeCounts(items []gjson.Result, path string) string {
	counts := make(map[string]int)
	for _, item := range items {
		itemType := strings.TrimSpace(item.Get(path).String())
		if itemType == "" {
			itemType = "<missing>"
		}
		counts[itemType]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ",")
}

func compactionDiagnosticRefs(items gjson.Result) []string {
	if !items.IsArray() {
		return nil
	}
	refs := make([]string, 0)
	for index, item := range items.Array() {
		if item.Get("type").String() != "compaction" {
			continue
		}
		content := item.Get("encrypted_content").String()
		itemID := strings.TrimSpace(item.Get("id").String())
		if itemID == "" {
			itemID = "<missing>"
		}
		refs = append(refs, fmt.Sprintf("index=%d,id=%s,chars=%d,sha256=%s", index, itemID, len(content), compactionDiagnosticSHA256([]byte(content))))
	}
	return refs
}

func setCompactionDiagnosticString(fields log.Fields, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		fields[key] = value
	}
}

func compactionDiagnosticSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}
