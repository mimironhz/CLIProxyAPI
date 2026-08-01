package helps

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	toolSearchToolName       = "tool_search"
	toolSearchHostedType     = "tool_search"
	toolSearchCallItemType   = "tool_search_call"
	toolSearchOutputItemType = "tool_search_output"
	toolSearchNamespaceType  = "namespace"
	toolSearchFunctionType   = "function"
	sseDataTag               = "data: "
)

// toolSearchFunctionJSON is the model-callable stand-in for Codex's hosted
// tool_search tool. Upstreams that speak plain Chat Completions have no hosted
// tool types, so the tool is forwarded as an ordinary function and the model's
// call is converted back into a tool_search_call item on the response path
// (RestoreToolSearchStreamChunk / RestoreToolSearchResponse) for Codex Desktop's
// own loader to resolve. Without this the deferred children of the codex_app
// namespace — send_message_to_thread, wait_threads, handoff_thread and the rest
// — can never be loaded.
const toolSearchFunctionJSON = `{"type":"function","name":"tool_search","description":"Search for and load tools that are not currently in your tool list. Provide a natural-language 'query' describing the tool or capability you need (for example a tool name a skill tells you to use, such as send_message_to_thread or mcp__node_repl__js). Matching tools are loaded and become callable on your next step. Call this whenever a skill or instruction references a tool you do not currently have.","parameters":{"type":"object","properties":{"query":{"type":"string","description":"Natural-language description of the tool or capability to find."},"limit":{"type":"integer","description":"Maximum number of tools to load (optional)."}},"required":["query"],"additionalProperties":false}}`

// PrepareResponsesToolSearch rewrites a Responses-shaped request so a Chat
// Completions upstream can take part in Codex's deferred-tool loading: the
// hosted tool_search declaration becomes a flat function, tools the client
// already loaded are merged back into the tools array, and the client-side
// round-trip items become an ordinary function call plus its result so the
// transcript records that the search happened. It must run before the request is
// translated, while the payload still carries Responses "input" items.
func PrepareResponsesToolSearch(body []byte) []byte {
	if !gjson.ValidBytes(body) {
		return body
	}
	body = hoistToolSearchTool(body)
	body = mergeToolSearchLoadedTools(body)
	return RewriteToolSearchInputItems(body)
}

// hoistToolSearchTool replaces every hosted tool_search declaration — top level
// or namespace child — with a single flat function at the top level. Codex can
// declare it in both places, and a namespace child would otherwise be forwarded
// under its qualified "<namespace>__tool_search" name, which the response path
// no longer recognizes.
func hoistToolSearchTool(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}

	found := false
	kept := make([][]byte, 0, len(tools.Array()))
	for _, tool := range tools.Array() {
		toolType := strings.TrimSpace(tool.Get("type").String())
		if toolType == toolSearchHostedType || isFlatToolSearchFunction(tool) {
			found = true
			continue
		}
		if toolType != toolSearchNamespaceType {
			kept = append(kept, []byte(tool.Raw))
			continue
		}

		children := tool.Get("tools")
		if !children.IsArray() {
			kept = append(kept, []byte(tool.Raw))
			continue
		}
		keptChildren := make([][]byte, 0, len(children.Array()))
		childDropped := false
		for _, child := range children.Array() {
			if strings.TrimSpace(child.Get("type").String()) == toolSearchHostedType || isFlatToolSearchFunction(child) {
				found = true
				childDropped = true
				continue
			}
			keptChildren = append(keptChildren, []byte(child.Raw))
		}
		if !childDropped {
			kept = append(kept, []byte(tool.Raw))
			continue
		}
		updated, errSet := sjson.SetRawBytes([]byte(tool.Raw), "tools", JoinRawJSONArray(keptChildren))
		if errSet != nil {
			kept = append(kept, []byte(tool.Raw))
			continue
		}
		kept = append(kept, updated)
	}
	if !found {
		return body
	}

	kept = append(kept, []byte(toolSearchFunctionJSON))
	updated, errSet := sjson.SetRawBytes(body, "tools", JoinRawJSONArray(kept))
	if errSet != nil {
		return body
	}
	return updated
}

func isFlatToolSearchFunction(tool gjson.Result) bool {
	toolType := strings.TrimSpace(tool.Get("type").String())
	if toolType != toolSearchFunctionType && toolType != "" {
		return false
	}
	return strings.TrimSpace(tool.Get("name").String()) == toolSearchToolName
}

// mergeToolSearchLoadedTools folds tools the client loaded through tool_search
// back into the tools array. They arrive only inside tool_search_output items
// and are never re-added to "tools", so without this the model never sees the
// tool it just asked for. Namespace entries are merged child-by-child: the
// loaded codex_app namespace carries the deferred thread tools while the base
// request already declares the same namespace with its eager children, and
// replacing rather than merging would drop one set or the other.
func mergeToolSearchLoadedTools(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	harvested := make([]gjson.Result, 0)
	for _, item := range input.Array() {
		if item.Get("type").String() != toolSearchOutputItemType {
			continue
		}
		if tools := item.Get("tools"); tools.IsArray() {
			harvested = append(harvested, tools.Array()...)
		}
	}
	if len(harvested) == 0 {
		return body
	}

	existing := gjson.GetBytes(body, "tools")
	merged := make([][]byte, 0, len(existing.Array())+len(harvested))
	indexByKey := make(map[string]int)
	if existing.IsArray() {
		for _, tool := range existing.Array() {
			indexByKey[toolSearchToolKey(tool)] = len(merged)
			merged = append(merged, []byte(tool.Raw))
		}
	}

	changed := false
	for _, tool := range harvested {
		key := toolSearchToolKey(tool)
		position, exists := indexByKey[key]
		if !exists {
			indexByKey[key] = len(merged)
			merged = append(merged, []byte(tool.Raw))
			changed = true
			continue
		}
		if strings.TrimSpace(tool.Get("type").String()) != toolSearchNamespaceType {
			continue
		}
		updated, ok := mergeNamespaceChildren(merged[position], tool)
		if !ok {
			continue
		}
		merged[position] = updated
		changed = true
	}
	if !changed {
		return body
	}

	updated, errSet := sjson.SetRawBytes(body, "tools", JoinRawJSONArray(merged))
	if errSet != nil {
		return body
	}
	return updated
}

// mergeNamespaceChildren appends the loaded namespace's children to the existing
// declaration, skipping names it already carries.
func mergeNamespaceChildren(existingRaw []byte, loaded gjson.Result) ([]byte, bool) {
	loadedChildren := loaded.Get("tools")
	if !loadedChildren.IsArray() {
		return nil, false
	}
	existing := gjson.ParseBytes(existingRaw)
	children := make([][]byte, 0)
	seen := make(map[string]struct{})
	if current := existing.Get("tools"); current.IsArray() {
		for _, child := range current.Array() {
			seen[strings.TrimSpace(child.Get("name").String())] = struct{}{}
			children = append(children, []byte(child.Raw))
		}
	}
	added := false
	for _, child := range loadedChildren.Array() {
		name := strings.TrimSpace(child.Get("name").String())
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		children = append(children, []byte(child.Raw))
		added = true
	}
	if !added {
		return nil, false
	}
	updated, errSet := sjson.SetRawBytes(existingRaw, "tools", JoinRawJSONArray(children))
	if errSet != nil {
		return nil, false
	}
	return updated, true
}

func toolSearchToolKey(tool gjson.Result) string {
	toolType := strings.TrimSpace(tool.Get("type").String())
	if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
		if toolType == toolSearchNamespaceType {
			return "namespace:" + name
		}
		return "name:" + name
	}
	return "type:" + toolType
}

// RewriteToolSearchInputItems converts the tool_search round-trip into the plain
// function-call pair a Chat Completions transcript can carry.
//
// Deleting the pair outright — the previous behaviour — left the transcript
// frozen one step before the search. The assistant's last message announces that
// it is about to look the tools up and nothing records that it ever did, so the
// model reissues tool_search on every turn even though the loaded tools are
// already sitting in the tools array. Codex's own prompt makes that loop
// unbreakable: observed 35 consecutive searches with zero tool calls.
//
// A call is rewritten only when its output is present and vice versa. An orphan
// is dropped, because Chat Completions upstreams reject an assistant tool_calls
// message whose tool_call_id has no matching tool message (Kimi: "the following
// tool_call_ids did not have response messages"). The client legitimately sends
// an output-only delta, whose matching call arrives from the previous response's
// output, so both halves are present by the time this runs.
func RewriteToolSearchInputItems(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	items := input.Array()

	callIDs := make(map[string]struct{})
	outputIDs := make(map[string]struct{})
	for _, item := range items {
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" {
			continue
		}
		switch item.Get("type").String() {
		case toolSearchCallItemType:
			callIDs[callID] = struct{}{}
		case toolSearchOutputItemType:
			outputIDs[callID] = struct{}{}
		}
	}

	kept := make([][]byte, 0, len(items))
	changed := false
	for _, item := range items {
		itemType := item.Get("type").String()
		if itemType != toolSearchCallItemType && itemType != toolSearchOutputItemType {
			kept = append(kept, []byte(item.Raw))
			continue
		}
		changed = true

		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" {
			continue
		}
		if _, ok := callIDs[callID]; !ok {
			continue
		}
		if _, ok := outputIDs[callID]; !ok {
			continue
		}

		if itemType == toolSearchCallItemType {
			if converted, ok := toolSearchCallAsFunctionCall(item, callID); ok {
				kept = append(kept, converted)
			}
			continue
		}
		if converted, ok := toolSearchOutputAsFunctionCallOutput(item, callID); ok {
			kept = append(kept, converted)
		}
	}
	if !changed {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "input", JoinRawJSONArray(kept))
	if errSet != nil {
		return body
	}
	return updated
}

// toolSearchCallAsFunctionCall renders the hosted call as a call of the flat
// tool_search function that hoistToolSearchTool already declares upstream.
func toolSearchCallAsFunctionCall(item gjson.Result, callID string) ([]byte, bool) {
	out := []byte(`{"type":"function_call","name":"` + toolSearchToolName + `","arguments":"{}"}`)
	out, errSet := sjson.SetBytes(out, "call_id", callID)
	if errSet != nil {
		return nil, false
	}
	if id := strings.TrimSpace(item.Get("id").String()); id != "" {
		out, _ = sjson.SetBytes(out, "id", id)
	}
	if arguments := item.Get("arguments"); arguments.Exists() {
		raw := arguments.Raw
		if arguments.Type == gjson.String {
			raw = strings.TrimSpace(arguments.String())
		}
		if gjson.Valid(raw) {
			out, _ = sjson.SetBytes(out, "arguments", raw)
		}
	}
	return out, true
}

func toolSearchOutputAsFunctionCallOutput(item gjson.Result, callID string) ([]byte, bool) {
	out := []byte(`{"type":"function_call_output"}`)
	out, errSet := sjson.SetBytes(out, "call_id", callID)
	if errSet != nil {
		return nil, false
	}
	out, errSet = sjson.SetBytes(out, "output", toolSearchLoadedSummary(item.Get("tools")))
	if errSet != nil {
		return nil, false
	}
	return out, true
}

// toolSearchLoadedSummary names the loaded tools under the same qualified names
// they are forwarded upstream with, so the model can call them straight away
// rather than searching for them again.
func toolSearchLoadedSummary(tools gjson.Result) string {
	names := toolSearchLoadedNames(tools)
	if len(names) == 0 {
		return "No tools matched this query. Do not repeat the search; work with the tools you already have."
	}
	return fmt.Sprintf("Loaded %d tool(s). They are in your tool list now and can be called directly: %s", len(names), strings.Join(names, ", "))
}

func toolSearchLoadedNames(tools gjson.Result) []string {
	if !tools.IsArray() {
		return nil
	}
	names := make([]string, 0)
	for _, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) == toolSearchNamespaceType {
			namespaceName := strings.TrimSpace(tool.Get("name").String())
			children := tool.Get("tools")
			if !children.IsArray() {
				continue
			}
			for _, child := range children.Array() {
				if name := qualifyToolSearchName(namespaceName, strings.TrimSpace(child.Get("name").String())); name != "" {
					names = append(names, name)
				}
			}
			continue
		}
		if name := strings.TrimSpace(tool.Get("name").String()); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// qualifyToolSearchName mirrors the qualified "<namespace>__<child>" name the
// request translator forwards namespace children under.
func qualifyToolSearchName(namespaceName, childName string) string {
	if childName == "" || namespaceName == "" || strings.HasPrefix(childName, "mcp__") {
		return childName
	}
	if strings.HasPrefix(childName, namespaceName) {
		return childName
	}
	if strings.HasSuffix(namespaceName, "__") {
		return namespaceName + childName
	}
	return namespaceName + "__" + childName
}

// toToolSearchCallItem converts a call of the tool_search stand-in back into the
// hosted tool_search_call item Codex Desktop expects. execution=client makes
// Desktop's own loader resolve it and inject the tools it names, which then
// arrive on the next turn as a tool_search_output item.
func toToolSearchCallItem(item gjson.Result, final bool) ([]byte, bool) {
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
			candidate = strings.TrimSpace(raw.String())
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

// rewriteToolSearchItemAtPath converts a tool_search function call at path into a
// tool_search_call item, leaving every other item untouched.
func rewriteToolSearchItemAtPath(data []byte, path string, final bool) []byte {
	item := gjson.GetBytes(data, path)
	if !item.IsObject() || item.Get("type").String() != "function_call" {
		return data
	}
	if strings.TrimSpace(item.Get("name").String()) != toolSearchToolName {
		return data
	}
	converted, ok := toToolSearchCallItem(item, final)
	if !ok {
		return data
	}
	updated, errSet := sjson.SetRawBytes(data, path, converted)
	if errSet != nil {
		return data
	}
	return updated
}

// restoreToolSearchEvent rewrites a single Responses event object.
func restoreToolSearchEvent(data []byte) []byte {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return data
	}
	switch gjson.GetBytes(data, "type").String() {
	case "response.output_item.added":
		return rewriteToolSearchItemAtPath(data, "item", false)
	case "response.output_item.done":
		return rewriteToolSearchItemAtPath(data, "item", true)
	case "response.completed":
		output := gjson.GetBytes(data, "response.output")
		if !output.IsArray() {
			return data
		}
		for index := range output.Array() {
			data = rewriteToolSearchItemAtPath(data, fmt.Sprintf("response.output.%d", index), true)
		}
		return data
	}
	return data
}

// RestoreToolSearchStreamChunk rewrites the tool_search stand-in inside a
// translated SSE chunk so Codex receives a real tool_search_call. Chunks may
// carry event/data line pairs, so every data line is rewritten in place.
func RestoreToolSearchStreamChunk(chunk []byte) []byte {
	if len(chunk) == 0 || !bytes.Contains(chunk, []byte(toolSearchToolName)) {
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
		restored := restoreToolSearchEvent(payload)
		if bytes.Equal(restored, payload) {
			continue
		}
		lines[i] = append([]byte(sseDataTag), restored...)
		changed = true
	}
	if !changed {
		return chunk
	}
	return bytes.Join(lines, []byte("\n"))
}

// RestoreToolSearchResponse rewrites the tool_search stand-in inside a
// translated non-stream Responses body.
func RestoreToolSearchResponse(body []byte) []byte {
	if len(body) == 0 || !bytes.Contains(body, []byte(toolSearchToolName)) || !gjson.ValidBytes(body) {
		return body
	}
	output := gjson.GetBytes(body, "output")
	if !output.IsArray() {
		return body
	}
	for index := range output.Array() {
		body = rewriteToolSearchItemAtPath(body, fmt.Sprintf("output.%d", index), true)
	}
	return body
}
