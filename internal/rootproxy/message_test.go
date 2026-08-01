package rootproxy

import "testing"

func TestInspectFirstClientMessage(t *testing.T) {
	payload := []byte(" { \"unknown\" : {\"nested\":[1,2]}, \"type\" : \"response.create\", \"model\" : \"gpt-stock\" } \n")
	model, errInspect := inspectFirstClientMessage(payload)
	if errInspect != nil {
		t.Fatalf("inspectFirstClientMessage() error = %v", errInspect)
	}
	if model != "gpt-stock" {
		t.Fatalf("model = %q", model)
	}
}

func TestInspectFirstClientMessageRejectsAmbiguousOrInvalidJSON(t *testing.T) {
	tests := map[string]string{
		"array":            `[]`,
		"missing type":     `{"model":"gpt-stock"}`,
		"wrong type":       `{"type":"response.append","model":"gpt-stock"}`,
		"missing model":    `{"type":"response.create"}`,
		"empty model":      `{"type":"response.create","model":""}`,
		"non-string model": `{"type":"response.create","model":42}`,
		"duplicate model":  `{"type":"response.create","model":"gpt-stock","model":"relay-model"}`,
		"duplicate type":   `{"type":"response.create","type":"response.create","model":"gpt-stock"}`,
		"trailing value":   `{"type":"response.create","model":"gpt-stock"} {}`,
		"truncated":        `{"type":"response.create","model":"gpt-stock"`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, errInspect := inspectFirstClientMessage([]byte(raw)); errInspect == nil {
				t.Fatal("inspectFirstClientMessage() succeeded")
			}
		})
	}
}

func TestInspectClientMessageAllowsContinuationWithoutModel(t *testing.T) {
	envelope, errInspect := inspectClientMessage([]byte(`{"type":"response.create","previous_response_id":"resp_1","input":[]}`))
	if errInspect != nil {
		t.Fatalf("inspectClientMessage() error = %v", errInspect)
	}
	if envelope.hasModel {
		t.Fatalf("hasModel = true, model = %q", envelope.model)
	}
}

func TestInspectClientMessageStateReferences(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		stateful  bool
		wantError bool
	}{
		{name: "previous response", payload: `{"previous_response_id":"resp_1"}`, stateful: true},
		{name: "empty previous response", payload: `{"previous_response_id":""}`, stateful: true},
		{name: "null previous response", payload: `{"previous_response_id":null}`},
		{name: "conversation id", payload: `{"conversation":"conv_1"}`, stateful: true},
		{name: "conversation object", payload: `{"conversation":{"id":"conv_1"}}`, stateful: true},
		{name: "empty conversation id", payload: `{"conversation":""}`, stateful: true},
		{name: "null conversation", payload: `{"conversation":null}`},
		{name: "duplicate previous response", payload: `{"previous_response_id":"resp_1","previous_response_id":"resp_2"}`, wantError: true},
		{name: "numeric previous response", payload: `{"previous_response_id":42}`, wantError: true},
		{name: "object previous response", payload: `{"previous_response_id":{}}`, wantError: true},
		{name: "duplicate conversation", payload: `{"conversation":"conv_1","conversation":"conv_2"}`, wantError: true},
		{name: "numeric conversation", payload: `{"conversation":42}`, wantError: true},
		{name: "boolean conversation", payload: `{"conversation":true}`, wantError: true},
		{name: "array conversation", payload: `{"conversation":[]}`, wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope, errInspect := inspectClientMessage([]byte(test.payload))
			if test.wantError {
				if errInspect == nil {
					t.Fatal("inspectClientMessage() succeeded")
				}
				return
			}
			if errInspect != nil {
				t.Fatalf("inspectClientMessage() error = %v", errInspect)
			}
			if got := envelope.referencesUpstreamState(); got != test.stateful {
				t.Fatalf("referencesUpstreamState() = %t, want %t", got, test.stateful)
			}
		})
	}
}

func TestUpstreamEventIsTerminal(t *testing.T) {
	terminal := map[string]string{
		`{"type":"response.completed"}`:  "completed",
		`{"type":"response.failed"}`:     "failed",
		`{"type":"response.incomplete"}`: "incomplete",
		`{"type":"response.cancelled"}`:  "canceled",
		`{"type":"response.canceled"}`:   "canceled",
		`{"type":"response.done"}`:       "completed",
	}
	for payload, expectedOutcome := range terminal {
		if !upstreamEventIsTerminal([]byte(payload)) {
			t.Errorf("upstreamEventIsTerminal(%s) = false", payload)
		}
		outcome, ok := upstreamTerminalOutcome([]byte(payload))
		if !ok || outcome != expectedOutcome {
			t.Errorf("upstreamTerminalOutcome(%s) = %q, %t; want %q, true", payload, outcome, ok, expectedOutcome)
		}
	}

	nonTerminal := []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_text.delta"}`,
		`{"type":"error"}`,
		`{"type":"response.completed","type":"response.done"}`,
		`{"type":42}`,
		`{}`,
		`not-json`,
	}
	for _, payload := range nonTerminal {
		if upstreamEventIsTerminal([]byte(payload)) {
			t.Errorf("upstreamEventIsTerminal(%s) = true", payload)
		}
	}
}

func TestUpstreamEventIsError(t *testing.T) {
	if !upstreamEventIsError([]byte(`{"type":"error","status":400}`)) {
		t.Fatal("upstream error event was not detected")
	}
	for _, payload := range []string{`{"type":"response.failed"}`, `{"type":"error","type":"error"}`, `not-json`} {
		if upstreamEventIsError([]byte(payload)) {
			t.Fatalf("non-error payload was detected: %s", payload)
		}
	}
}
