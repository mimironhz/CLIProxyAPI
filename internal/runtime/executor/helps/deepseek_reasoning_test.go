package helps

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestIsDeepSeekBaseURL(t *testing.T) {
	tests := map[string]bool{
		"https://api.deepseek.com/v1":  true,
		"https://api.deepseek.com":     true,
		"https://deepseek.com/v1":      true,
		"https://openrouter.ai/api/v1": false,
		"https://api.moonshot.cn/v1":   false,
		"https://deepseek.com.evil.io": false,
		"":                             false,
		"://broken":                    false,
	}
	for baseURL, want := range tests {
		if got := IsDeepSeekBaseURL(baseURL); got != want {
			t.Errorf("IsDeepSeekBaseURL(%q) = %v, want %v", baseURL, got, want)
		}
	}
}

func TestEnsureDeepSeekReasoningContentBackfillsToolCallTurns(t *testing.T) {
	translated := []byte(`{"messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":"thinking out loud"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"42"}
	]}`)
	out := EnsureDeepSeekReasoningContent(translated, []byte(`{"input":[]}`))

	messages := gjson.GetBytes(out, "messages").Array()
	if messages[1].Get("reasoning_content").Exists() {
		t.Error("plain assistant text message gained reasoning_content; DeepSeek does not require it")
	}
	reasoning := messages[2].Get("reasoning_content")
	if !reasoning.Exists() {
		t.Fatal("assistant tool_calls message is still missing reasoning_content")
	}
	if reasoning.String() != "" {
		t.Errorf("reasoning_content = %q, want an empty string when the client sent no reasoning", reasoning.String())
	}
	if messages[3].Get("reasoning_content").Exists() {
		t.Error("tool message gained reasoning_content")
	}
}

func TestEnsureDeepSeekReasoningContentRecoversClientReasoning(t *testing.T) {
	responses := []byte(`{"input":[
		{"type":"reasoning","summary":[{"type":"summary_text","text":"I should call f."}]},
		{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":"42"}
	]}`)
	translated := []byte(`{"messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}
	]}`)
	out := EnsureDeepSeekReasoningContent(translated, responses)

	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "I should call f." {
		t.Errorf("reasoning_content = %q, want the client's reasoning summary", got)
	}
}

func TestEnsureDeepSeekReasoningContentPreservesExisting(t *testing.T) {
	translated := []byte(`{"messages":[
		{"role":"assistant","content":null,"reasoning_content":"kept","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}
	]}`)
	out := EnsureDeepSeekReasoningContent(translated, []byte(`{"input":[]}`))
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "kept" {
		t.Errorf("reasoning_content = %q, want the original value untouched", got)
	}
}

// The compaction path forwards a Responses-shaped body, which has no messages
// array to patch.
func TestEnsureDeepSeekReasoningContentIgnoresNonChatBody(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"hi"}]}`)
	if out := EnsureDeepSeekReasoningContent(body, body); string(out) != string(body) {
		t.Errorf("body was modified: %s", string(out))
	}
}

func TestSealDeepSeekReasoningNonStream(t *testing.T) {
	body := []byte(`{"output":[
		{"type":"reasoning","encrypted_content":"","summary":[{"type":"summary_text","text":"I should call f."}]},
		{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}
	]}`)
	out := SealDeepSeekReasoning(body)

	sealed := gjson.GetBytes(out, "output.0.encrypted_content").String()
	if !strings.HasPrefix(sealed, deepSeekReasoningPrefix) {
		t.Fatalf("encrypted_content = %q, want the proxy prefix", sealed)
	}
	if got := unsealDeepSeekReasoning(sealed); got != "I should call f." {
		t.Errorf("unsealed = %q, want the original summary", got)
	}
	if gjson.GetBytes(out, "output.1.encrypted_content").Exists() {
		t.Error("function_call item gained encrypted_content")
	}
}

// A blob the upstream genuinely issued must survive untouched.
func TestSealDeepSeekReasoningPreservesRealBlob(t *testing.T) {
	body := []byte(`{"output":[{"type":"reasoning","encrypted_content":"gAAAAAreal","summary":[{"type":"summary_text","text":"x"}]}]}`)
	if got := gjson.GetBytes(SealDeepSeekReasoning(body), "output.0.encrypted_content").String(); got != "gAAAAAreal" {
		t.Errorf("encrypted_content = %q, want the upstream blob untouched", got)
	}
}

func TestSealDeepSeekReasoningStreamChunk(t *testing.T) {
	chunk := []byte("event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"","summary":[{"type":"summary_text","text":"thought"}]}}` + "\n")
	out := SealDeepSeekReasoningStreamChunk(chunk)

	line := strings.TrimPrefix(strings.Split(string(out), "\n")[1], sseDataTag)
	sealed := gjson.Get(line, "item.encrypted_content").String()
	if got := unsealDeepSeekReasoning(sealed); got != "thought" {
		t.Errorf("unsealed = %q, want %q", got, "thought")
	}
}

// The added event fires before any summary text exists; sealing an empty
// summary would mint a blob that decodes to nothing.
func TestSealDeepSeekReasoningSkipsEmptySummary(t *testing.T) {
	chunk := []byte(`data: {"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"","summary":[]}}` + "\n")
	if out := SealDeepSeekReasoningStreamChunk(chunk); string(out) != string(chunk) {
		t.Errorf("chunk was modified: %s", string(out))
	}
}

func TestSealDeepSeekReasoningCompletedEvent(t *testing.T) {
	chunk := []byte(`data: {"type":"response.completed","response":{"output":[{"type":"reasoning","encrypted_content":"","summary":[{"type":"summary_text","text":"done thinking"}]}]}}` + "\n")
	out := SealDeepSeekReasoningStreamChunk(chunk)
	line := strings.TrimPrefix(strings.TrimSpace(string(out)), sseDataTag)
	if got := unsealDeepSeekReasoning(gjson.Get(line, "response.output.0.encrypted_content").String()); got != "done thinking" {
		t.Errorf("unsealed = %q, want %q", got, "done thinking")
	}
}

// Round trip: a sealed item replayed by the client must yield reasoning_content
// on the assistant tool-call message, which is the whole point of sealing.
func TestSealedReasoningSurvivesReplay(t *testing.T) {
	sealed := SealDeepSeekReasoning([]byte(
		`{"output":[{"type":"reasoning","encrypted_content":"","summary":[{"type":"summary_text","text":"call f first"}]}]}`))
	blob := gjson.GetBytes(sealed, "output.0.encrypted_content").String()

	// The client replays the item; some drop the summary and keep only the blob.
	responses := []byte(`{"input":[
		{"type":"reasoning","encrypted_content":"` + blob + `","summary":[]},
		{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}
	]}`)
	translated := []byte(`{"messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}
	]}`)
	if got := gjson.GetBytes(EnsureDeepSeekReasoningContent(translated, responses), "messages.0.reasoning_content").String(); got != "call f first" {
		t.Errorf("reasoning_content = %q, want the reasoning recovered from the sealed blob", got)
	}
}

// A blob minted by another provider must not be mistaken for ours.
func TestUnsealDeepSeekReasoningRejectsForeignBlob(t *testing.T) {
	for _, foreign := range []string{"gAAAAAopaque", "kimi-compaction-v1:aGk=", "", "deepseek-reasoning-v1:!!notbase64"} {
		if got := unsealDeepSeekReasoning(foreign); got != "" {
			t.Errorf("unsealDeepSeekReasoning(%q) = %q, want empty", foreign, got)
		}
	}
}
