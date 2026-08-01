package helps

import (
	"testing"

	"github.com/tidwall/gjson"
)

// The failure this repairs: codex_app's automation_update declares a bare root
// union, and the upstream flattens namespace children itself before validating
// each one, so an untouched child kills the whole request.
func TestNormalizeResponsesToolSchemasStampsNamespaceChild(t *testing.T) {
	body := []byte(`{"tools":[{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"automation_update","parameters":{"oneOf":[{"type":"object","properties":{}}],"$defs":{}}}]}]}`)

	out := NormalizeResponsesToolSchemas(body)

	parameters := gjson.GetBytes(out, "tools.0.tools.0.parameters")
	if got := parameters.Get("type").String(); got != "object" {
		t.Errorf("root type = %q, want object: %s", got, string(out))
	}
	if !parameters.Get("oneOf").IsArray() {
		t.Errorf("normalization dropped the original union: %s", string(out))
	}
	if !parameters.Get("$defs").IsObject() {
		t.Errorf("normalization dropped $defs: %s", string(out))
	}
}

func TestNormalizeResponsesToolSchemasStampsTopLevelTool(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"probe","parameters":{"anyOf":[{"properties":{}}]}}]}`)

	out := NormalizeResponsesToolSchemas(body)

	if got := gjson.GetBytes(out, "tools.0.parameters.type").String(); got != "object" {
		t.Errorf("root type = %q, want object: %s", got, string(out))
	}
	// Only the root is stamped; branch types are the upstream's business and
	// rewriting them would narrow schemas the upstream already accepts.
	if gjson.GetBytes(out, "tools.0.parameters.anyOf.0.type").Exists() {
		t.Errorf("normalization reached into a union branch: %s", string(out))
	}
}

func TestNormalizeResponsesToolSchemasStampsNullRootType(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"probe","parameters":{"type":null,"properties":{}}}]}`)

	out := NormalizeResponsesToolSchemas(body)

	if got := gjson.GetBytes(out, "tools.0.parameters.type").String(); got != "object" {
		t.Errorf("root type = %q, want object: %s", got, string(out))
	}
}

func TestNormalizeResponsesToolSchemasLeavesDeclaredTypesAlone(t *testing.T) {
	// A union type such as ["object","null"] is a deliberate declaration that
	// stamping would silently narrow.
	body := []byte(`{"tools":[{"type":"function","name":"typed","parameters":{"type":"object","properties":{}}},{"type":"function","name":"union","parameters":{"type":["object","null"]}}]}`)

	out := NormalizeResponsesToolSchemas(body)

	if string(out) != string(body) {
		t.Errorf("declared root types were rewritten:\n got %s\nwant %s", string(out), string(body))
	}
}

// Hosted tools and namespace containers carry no parameters at all, and the
// upstream accepts a function without them, so none may be invented.
func TestNormalizeResponsesToolSchemasLeavesParameterlessToolsAlone(t *testing.T) {
	body := []byte(`{"tools":[{"type":"web_search"},{"type":"function","name":"bare"},{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}`)

	out := NormalizeResponsesToolSchemas(body)

	if string(out) != string(body) {
		t.Errorf("parameterless tools were rewritten:\n got %s\nwant %s", string(out), string(body))
	}
}

func TestNormalizeResponsesToolSchemasIgnoresMalformedBodies(t *testing.T) {
	for name, body := range map[string]string{
		"empty":        "",
		"invalid json": `{"tools":`,
		"no tools":     `{"model":"m","input":[]}`,
		"tools object": `{"tools":{"type":"function"}}`,
	} {
		if out := NormalizeResponsesToolSchemas([]byte(body)); string(out) != body {
			t.Errorf("%s: body was rewritten to %s", name, string(out))
		}
	}
}
