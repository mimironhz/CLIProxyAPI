package rootproxy

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/tidwall/gjson"
)

func validGPTReasoningEncryptedContentForRootTest() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for index := 9; index < len(payload); index++ {
		payload[index] = byte(index)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func validGrokEncryptedContentForRootTest() string {
	payload := make([]byte, 128)
	for index := range payload {
		payload[index] = byte(index*73 + 19)
	}
	return base64.RawStdEncoding.EncodeToString(payload)
}

func validKimiCompactionForRootTest(summary string) string {
	return kimiCompactionPrefix + base64.StdEncoding.EncodeToString([]byte(summary))
}

func TestPrepareOfficialPayloadOrdinaryRequestPreservesBytes(t *testing.T) {
	payload := []byte(" { \"model\" : \"gpt-stock\", \"input\" : [ { \"type\" : \"message\", \"role\" : \"user\", \"content\" : \"hello\" } ] } \n")
	got, errPrepare := prepareOfficialPayload(payload)
	if errPrepare != nil {
		t.Fatalf("prepareOfficialPayload() error = %v", errPrepare)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("prepareOfficialPayload() changed ordinary bytes\ngot  = %s\nwant = %s", got, payload)
	}
	if len(got) > 0 && &got[0] != &payload[0] {
		t.Fatal("prepareOfficialPayload() did not return the original byte slice")
	}
}

func TestPrepareOfficialPayloadStripsForeignReasoningState(t *testing.T) {
	tests := []struct {
		name       string
		store      bool
		wantReason string
	}{
		{name: "store disabled", store: false},
		{name: "store enabled", store: true, wantReason: "rs_foreign"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := "false"
			if test.store {
				store = "true"
			}
			payload := []byte(`{"store":` + store + `,"input":[` +
				`{"id":"rs_foreign","type":"reasoning","encrypted_content":"foreign-state","summary":[]},` +
				`{"id":"msg_1","type":"message","role":"user","content":"hello"}` +
				`]}`)

			got, errPrepare := prepareOfficialPayload(payload)
			if errPrepare != nil {
				t.Fatalf("prepareOfficialPayload() error = %v", errPrepare)
			}
			if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
				t.Fatalf("foreign encrypted_content remains: %s", got)
			}
			if gotID := gjson.GetBytes(got, "input.0.id").String(); gotID != test.wantReason {
				t.Fatalf("reasoning id = %q, want %q; body=%s", gotID, test.wantReason, got)
			}
			if gotID := gjson.GetBytes(got, "input.1.id").String(); gotID != "msg_1" {
				t.Fatalf("ordinary message id = %q, want msg_1; body=%s", gotID, got)
			}
		})
	}
}

func TestPrepareOfficialPayloadPreservesValidGPTOpaqueState(t *testing.T) {
	valid := validGPTReasoningEncryptedContentForRootTest()
	for _, itemType := range []string{"reasoning", "compaction"} {
		t.Run(itemType, func(t *testing.T) {
			payload := []byte(`{"store":false,"input":[{"id":"opaque_1","type":"` + itemType + `","encrypted_content":"` + valid + `","summary":[]}]}`)
			got, errPrepare := prepareOfficialPayload(payload)
			if errPrepare != nil {
				t.Fatalf("prepareOfficialPayload() error = %v", errPrepare)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("valid GPT state changed\ngot  = %s\nwant = %s", got, payload)
			}
			if len(got) > 0 && &got[0] != &payload[0] {
				t.Fatal("valid GPT state did not return the original byte slice")
			}
		})
	}
}

func TestPrepareOfficialPayloadRejectsForeignCompaction(t *testing.T) {
	for _, encryptedContent := range []string{"foreign-state", "", "gAAAAABinvalid-gpt-shape"} {
		t.Run(encryptedContent, func(t *testing.T) {
			payload := []byte(`{"input":[{"id":"cmp_1","type":"compaction","encrypted_content":"` + encryptedContent + `"},{"type":"message","role":"user","content":"hello"}]}`)
			got, errPrepare := prepareOfficialPayload(payload)
			if !errors.Is(errPrepare, errNonPortableCompaction) {
				t.Fatalf("prepareOfficialPayload() error = %v, want %v", errPrepare, errNonPortableCompaction)
			}
			if got != nil {
				t.Fatalf("prepareOfficialPayload() body = %s, want nil", got)
			}
		})
	}
}

func TestValidateRelayPayloadStateUsesExactProviderProvenance(t *testing.T) {
	grok := validGrokEncryptedContentForRootTest()
	kimi := validKimiCompactionForRootTest("summary")
	gpt := validGPTReasoningEncryptedContentForRootTest()

	tests := []struct {
		name              string
		payload           string
		target            relayProvider
		createsCompaction bool
		wantError         bool
	}{
		{name: "xai accepts Grok", payload: `{"input":[{"type":"compaction","encrypted_content":"` + grok + `"}]}`, target: relayProviderXAI},
		{name: "kimi accepts Kimi", payload: `{"input":[{"type":"compaction","encrypted_content":"` + kimi + `"}]}`, target: relayProviderKimi},
		{name: "xai rejects Kimi", payload: `{"input":[{"type":"compaction","encrypted_content":"` + kimi + `"}]}`, target: relayProviderXAI, wantError: true},
		{name: "kimi rejects Grok", payload: `{"input":[{"type":"compaction","encrypted_content":"` + grok + `"}]}`, target: relayProviderKimi, wantError: true},
		{name: "relay rejects GPT", payload: `{"input":[{"type":"compaction","encrypted_content":"` + gpt + `"}]}`, target: relayProviderXAI, wantError: true},
		{name: "mixed providers rejected", payload: `{"input":[{"type":"compaction","encrypted_content":"` + grok + `"},{"type":"compaction","encrypted_content":"` + kimi + `"}]}`, target: relayProviderXAI, wantError: true},
		{name: "unknown state rejected", payload: `{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`, target: relayProviderXAI, wantError: true},
		{name: "missing state rejected", payload: `{"input":[{"type":"compaction"}]}`, target: relayProviderXAI, wantError: true},
		{name: "ordinary unclassified turn accepted", payload: `{"input":[{"type":"message","role":"user","content":"hi"}]}`},
		{name: "unclassified replay rejected", payload: `{"input":[{"type":"compaction","encrypted_content":"` + kimi + `"}]}`, wantError: true},
		{name: "unclassified trigger rejected", payload: `{"input":[{"type":"compaction_trigger"}]}`, wantError: true},
		{name: "unclassified compact endpoint rejected", payload: `{"input":[]}`, createsCompaction: true, wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errValidate := validateRelayPayloadState([]byte(test.payload), test.target, test.createsCompaction)
			if (errValidate != nil) != test.wantError {
				t.Fatalf("validateRelayPayloadState() error = %v, wantError %t", errValidate, test.wantError)
			}
		})
	}
}

// A DeepSeek reasoning blob minted by the Relay must not reach the official
// upstream: it is not GPT state, so only the state is stripped and the turn
// survives.
func TestPrepareOfficialPayloadStripsSealedDeepSeekReasoning(t *testing.T) {
	payload := []byte(`{"store":false,"input":[
		{"type":"reasoning","id":"rs_1","encrypted_content":"deepseek-reasoning-v1:Y2FsbCBmIGZpcnN0","summary":[]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	]}`)
	out, errPrepare := prepareOfficialPayload(payload)
	if errPrepare != nil {
		t.Fatalf("prepareOfficialPayload() error = %v", errPrepare)
	}
	if got := gjson.GetBytes(out, "input.0.encrypted_content"); got.Exists() {
		t.Errorf("foreign reasoning state survived: %q", got.String())
	}
	if got := gjson.GetBytes(out, "input.0.id"); got.Exists() {
		t.Errorf("orphan reasoning id survived with store=false: %q", got.String())
	}
	if got := gjson.GetBytes(out, "input.1.type").String(); got != "message" {
		t.Errorf("input.1.type = %q, want the user message preserved", got)
	}
}

func TestApplyOfficialFastServiceTier(t *testing.T) {
	fast := map[string]struct{}{"gpt-fast": {}}
	tests := []struct {
		name       string
		payload    string
		model      string
		fastModels map[string]struct{}
		wantTier   string
		wantSame   bool
	}{
		{
			name:       "configured model is forced onto the priority tier",
			payload:    `{"model":"gpt-fast","input":[]}`,
			model:      "gpt-fast",
			fastModels: fast,
			wantTier:   officialFastServiceTier,
		},
		{
			name:       "a client tier is overridden",
			payload:    `{"model":"gpt-fast","service_tier":"default","input":[]}`,
			model:      "gpt-fast",
			fastModels: fast,
			wantTier:   officialFastServiceTier,
		},
		{
			name:       "an already fast turn keeps its exact bytes",
			payload:    `{"model":"gpt-fast","service_tier":"priority","input":[]}`,
			model:      "gpt-fast",
			fastModels: fast,
			wantTier:   officialFastServiceTier,
			wantSame:   true,
		},
		{
			name:       "an unconfigured model is untouched",
			payload:    `{"model":"gpt-standard","input":[]}`,
			model:      "gpt-standard",
			fastModels: fast,
			wantSame:   true,
		},
		{
			name:     "an empty configuration is a no-op",
			payload:  `{"model":"gpt-fast","input":[]}`,
			model:    "gpt-fast",
			wantSame: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(test.payload)
			got, errApply := applyOfficialFastServiceTier(payload, test.model, test.fastModels)
			if errApply != nil {
				t.Fatalf("applyOfficialFastServiceTier() error = %v", errApply)
			}
			if gotTier := gjson.GetBytes(got, "service_tier").String(); gotTier != test.wantTier {
				t.Fatalf("service_tier = %q, want %q; body=%s", gotTier, test.wantTier, got)
			}
			if test.wantSame && !bytes.Equal(got, payload) {
				t.Fatalf("payload was rewritten\ngot  = %s\nwant = %s", got, payload)
			}
		})
	}
}
