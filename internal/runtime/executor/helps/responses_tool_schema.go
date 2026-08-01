package helps

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeResponsesToolSchemas stamps the root "type" onto tool parameter
// schemas that omit it, walking namespace children as well as top-level tools.
//
// Codex declares tools whose parameters are a bare root union — codex_app's
// automation_update is {"oneOf":[...],"$defs":{...}} — which OpenAI's own
// Responses API accepts. Third-party Responses upstreams do not: DeepSeek
// rejects the entire request with 400 `Invalid schema for function
// 'codex_app__automation_update': schema must be a JSON Schema of 'type:
// "object"', got 'type: null'`, so every turn that declares the tool dies
// before the model is reached. The upstream flattens namespace children into
// "<namespace>__<child>" itself and validates each one, hence the walk into
// namespace tools.
//
// Only the root is stamped, because that is all the upstream inspects — probed
// 2026-07-31, an untyped oneOf branch under a typed root is accepted — and
// because function call arguments are always an object, so the added
// constraint preserves what the schema already meant. The equivalent
// normalization for Chat Completions upstreams lives in the Responses request
// translator, which a Responses upstream never reaches.
func NormalizeResponsesToolSchemas(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	for index, tool := range tools.Array() {
		path := fmt.Sprintf("tools.%d", index)
		body = normalizeResponsesToolSchemaAtPath(body, path, tool)
		children := tool.Get("tools")
		if !children.IsArray() {
			continue
		}
		for childIndex, child := range children.Array() {
			body = normalizeResponsesToolSchemaAtPath(body, fmt.Sprintf("%s.tools.%d", path, childIndex), child)
		}
	}
	return body
}

// normalizeResponsesToolSchemaAtPath stamps "type":"object" on a single tool's
// parameters when the schema is an object whose root type is absent or null. A
// declared type is left alone even when it is a union such as
// ["object","null"], which stamping would silently narrow.
func normalizeResponsesToolSchemaAtPath(body []byte, path string, tool gjson.Result) []byte {
	parameters := tool.Get("parameters")
	if !parameters.IsObject() {
		return body
	}
	if schemaType := parameters.Get("type"); schemaType.Exists() && schemaType.Type != gjson.Null {
		return body
	}
	updated, errSet := sjson.SetBytes(body, path+".parameters.type", "object")
	if errSet != nil {
		return body
	}
	return updated
}
