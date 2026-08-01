package executor

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	xaiToolSearchCallItemType   = "tool_search_call"
	xaiToolSearchOutputItemType = "tool_search_output"
	xaiToolSearchToolName       = "tool_search"
)

// xaiToolSearchFunctionJSON is the Grok-callable stand-in for Codex's hosted
// tool_search tool. cli-chat-proxy rejects the hosted type outright, so it is
// forwarded as a plain function. Grok's call is converted back into a
// tool_search_call item on the response path (restoreXAIToolSearchCalls) so
// Codex Desktop's own client-side loader resolves it and injects the deferred
// tools. Without this shim every skill that depends on a deferred tool (for
// example chrome:control-chrome needing mcp__node_repl__js) is unusable on Grok.
const xaiToolSearchFunctionJSON = `{"type":"function","name":"tool_search","description":"Search for and load tools that are not currently in your tool list. Provide a natural-language 'query' describing the tool or capability you need (for example a tool name a skill tells you to use, such as node_repl or mcp__node_repl__js). Matching tools are loaded and become callable on your next step. Call this whenever a skill or instruction references a tool you do not currently have.","parameters":{"type":"object","properties":{"query":{"type":"string","description":"Natural-language description of the tool or capability to find."},"limit":{"type":"integer","description":"Maximum number of tools to load (optional)."}},"required":["query"],"additionalProperties":false}}`

// stripXAIDeferLoading removes defer_loading from a tool declaration. Grok has
// no deferred-loading mechanism, so a forwarded tool that keeps the flag stays
// invisible to the model: it re-runs tool_search forever instead of calling the
// tool the client just loaded.
func stripXAIDeferLoading(raw []byte) ([]byte, bool) {
	if !gjson.GetBytes(raw, "defer_loading").Exists() {
		return raw, false
	}
	updated, errDelete := sjson.DeleteBytes(raw, "defer_loading")
	if errDelete != nil {
		return raw, false
	}
	return updated, true
}

// harvestXAILoadedTools returns the tool declarations the client loaded through
// tool_search. Those arrive only inside tool_search_output input items and are
// never re-added to the request's tools array, so without harvesting them Grok
// never sees the loaded tool. Returned tools are normalized to the same flat,
// active function shape as the rest of the tools array.
func harvestXAILoadedTools(body []byte) [][]byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return nil
	}
	collected := make([]byte, 0)
	count := 0
	for _, item := range input.Array() {
		if item.Get("type").String() != xaiToolSearchOutputItemType {
			continue
		}
		tools := item.Get("tools")
		if !tools.IsArray() {
			continue
		}
		for _, tool := range tools.Array() {
			if count > 0 {
				collected = append(collected, ',')
			}
			collected = append(collected, tool.Raw...)
			count++
		}
	}
	if count == 0 {
		return nil
	}
	wrapped := make([]byte, 0, len(collected)+2)
	wrapped = append(wrapped, '[')
	wrapped = append(wrapped, collected...)
	wrapped = append(wrapped, ']')

	normalized, _, ok := normalizeXAIToolArray(gjson.ParseBytes(wrapped))
	if !ok {
		return nil
	}
	// normalizeXAIToolArray reports changed=false when nothing needed rewriting,
	// in which case it returns no bytes and the input array is already correct.
	source := gjson.ParseBytes(wrapped)
	if len(normalized) > 0 {
		source = gjson.ParseBytes(normalized)
	}
	out := make([][]byte, 0, count)
	for _, tool := range source.Array() {
		out = append(out, []byte(tool.Raw))
	}
	return out
}

// mergeXAIHarvestedTools appends harvested tools to the request's tools array,
// skipping any whose name (or bare type, for hosted tools) is already present so
// repeated searches do not accumulate duplicates.
func mergeXAIHarvestedTools(body []byte, harvested [][]byte) []byte {
	if len(harvested) == 0 {
		return body
	}
	seen := make(map[string]struct{})
	key := func(tool gjson.Result) string {
		if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
			return "name:" + name
		}
		return "type:" + strings.TrimSpace(tool.Get("type").String())
	}

	existing := gjson.GetBytes(body, "tools")
	merged := make([][]byte, 0, len(existing.Array())+len(harvested))
	if existing.IsArray() {
		for _, tool := range existing.Array() {
			seen[key(tool)] = struct{}{}
			merged = append(merged, []byte(tool.Raw))
		}
	}
	added := false
	for _, raw := range harvested {
		toolKey := key(gjson.ParseBytes(raw))
		if _, ok := seen[toolKey]; ok {
			continue
		}
		seen[toolKey] = struct{}{}
		merged = append(merged, raw)
		added = true
	}
	if !added {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "tools", helps.JoinRawJSONArray(merged))
	if errSet != nil {
		return body
	}
	return updated
}

// dropXAIToolSearchInputItems removes the tool_search round-trip from the input
// history. It is pure client-side orchestration: the loaded tools have already
// been harvested into the tools array, and cli-chat-proxy's untagged-enum input
// deserialization rejects these item shapes.
// dropXAIToolSearchInputItems rewrites the tool_search round-trip into an
// ordinary function call and its result. Erasing it instead left the transcript
// stuck one step before the search, so the model reissued tool_search forever;
// see helps.RewriteToolSearchInputItems. The websocket path passes through to
// xAI untouched, so this only affects the HTTP path.
func dropXAIToolSearchInputItems(body []byte) []byte {
	return helps.RewriteToolSearchInputItems(body)
}

// dedupeXAIToolSearchTools collapses the tool_search shim to a single entry.
// Codex can declare the hosted tool_search tool both at the top level and inside
// a namespace, and each occurrence normalizes to the same flat function, which
// xAI rejects as a duplicate tool name.
func dedupeXAIToolSearchTools(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	items := tools.Array()
	kept := make([][]byte, 0, len(items))
	seen := false
	dropped := false
	for _, tool := range items {
		isToolSearch := tool.Get("type").String() == xaiFunctionToolType &&
			strings.TrimSpace(tool.Get("name").String()) == xaiToolSearchToolName
		if isToolSearch {
			if seen {
				dropped = true
				continue
			}
			seen = true
		}
		kept = append(kept, []byte(tool.Raw))
	}
	if !dropped {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "tools", helps.JoinRawJSONArray(kept))
	if errSet != nil {
		return body
	}
	return updated
}

// xaiToToolSearchCall converts Grok's call of the tool_search shim back into the
// hosted tool_search_call item Codex Desktop expects. The item is marked
// execution=client so Desktop's own loader resolves it and injects the deferred
// tools it names; those then arrive on the next turn as tool_search_output.
func xaiToToolSearchCall(item gjson.Result, final bool) ([]byte, bool) {
	out := []byte(`{"type":"tool_search_call","execution":"client"}`)
	status := "in_progress"
	if final {
		status = "completed"
	}
	out, errSet := sjson.SetBytes(out, "status", status)
	if errSet != nil {
		return nil, false
	}
	for _, field := range []string{"id", "call_id"} {
		value := item.Get(field)
		if !value.Exists() {
			continue
		}
		out, errSet = sjson.SetBytes(out, field, value.String())
		if errSet != nil {
			return nil, false
		}
	}

	arguments := []byte(`{}`)
	if raw := item.Get("arguments"); raw.Exists() {
		candidate := raw.Raw
		if raw.Type == gjson.String {
			candidate = coerceXAIToolArgumentsJSON(strings.TrimSpace(raw.String()))
		}
		if parsed := gjson.Parse(candidate); parsed.IsObject() {
			arguments = []byte(parsed.Raw)
		}
	}
	out, errSet = sjson.SetRawBytes(out, "arguments", arguments)
	if errSet != nil {
		return nil, false
	}
	return out, true
}

// rewriteXAIToolCallItemAtPath converts a tool_search function call into a
// tool_search_call item and coerces integer-typed arguments on every other
// function call. Returns the data unchanged when the path is not a function call.
func rewriteXAIToolCallItemAtPath(data []byte, path string, final bool) []byte {
	item := gjson.GetBytes(data, path)
	if !item.IsObject() || item.Get("type").String() != "function_call" {
		return data
	}
	if strings.TrimSpace(item.Get("name").String()) == xaiToolSearchToolName {
		converted, ok := xaiToToolSearchCall(item, final)
		if !ok {
			return data
		}
		updated, errSet := sjson.SetRawBytes(data, path, converted)
		if errSet != nil {
			return data
		}
		return updated
	}

	arguments := item.Get("arguments")
	if arguments.Type != gjson.String {
		return data
	}
	coerced := coerceXAIToolArgumentsJSON(arguments.String())
	if coerced == arguments.String() {
		return data
	}
	updated, errSet := sjson.SetBytes(data, path+".arguments", coerced)
	if errSet != nil {
		return data
	}
	return updated
}

// restoreXAIToolSearchCalls rewrites streamed output items so Grok's use of the
// tool_search shim reaches Codex as a real tool_search_call, and so tool call
// arguments carry the integer types Codex expects.
func restoreXAIToolSearchCalls(data []byte) []byte {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return data
	}
	switch gjson.GetBytes(data, "type").String() {
	case "response.output_item.added":
		return rewriteXAIToolCallItemAtPath(data, "item", false)
	case "response.output_item.done":
		return rewriteXAIToolCallItemAtPath(data, "item", true)
	case "response.function_call_arguments.done":
		arguments := gjson.GetBytes(data, "arguments")
		if arguments.Type != gjson.String {
			return data
		}
		coerced := coerceXAIToolArgumentsJSON(arguments.String())
		if coerced == arguments.String() {
			return data
		}
		updated, errSet := sjson.SetBytes(data, "arguments", coerced)
		if errSet != nil {
			return data
		}
		return updated
	case "response.completed":
		output := gjson.GetBytes(data, "response.output")
		if !output.IsArray() {
			return data
		}
		for index := range output.Array() {
			data = rewriteXAIToolCallItemAtPath(data, fmt.Sprintf("response.output.%d", index), true)
		}
		return data
	}
	return data
}

// applyXAIToolSearchRequest harvests client-loaded tools out of the tool_search
// history, merges them into the tools array, and drops the round-trip items.
// Harvesting reads the pre-normalization input, so callers must invoke this
// before the input items are rewritten.
func applyXAIToolSearchRequest(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	harvested := harvestXAILoadedTools(body)
	body = mergeXAIHarvestedTools(body, harvested)
	body = dedupeXAIToolSearchTools(body)
	return dropXAIToolSearchInputItems(body)
}
