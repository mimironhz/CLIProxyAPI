package executor

import (
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// sseFramePayload returns the JSON carried by an SSE frame.
func sseFramePayload(t *testing.T, frame []byte) []byte {
	t.Helper()
	for _, line := range strings.Split(string(frame), "\n") {
		if payload, ok := strings.CutPrefix(line, "data: "); ok {
			return []byte(payload)
		}
	}
	t.Fatalf("frame carries no data line: %s", frame)
	return nil
}

// Kimi For Coding is Chat Completions only, so compaction is synthesized from a
// summarization turn. The request that reaches Kimi must be an ordinary turn:
// the trigger is gone, the agent instructions are replaced, tools are disabled
// rather than removed (the transcript still references them), and the last turn
// asks for the summary.
func TestKimiBuildCompactionRequestBecomesSummarizationTurn(t *testing.T) {
	payload := []byte(`{"model":"kimi-k3","stream":true,"instructions":"You are Codex","text":{"verbosity":"low"},"tools":[{"type":"function","name":"shell"}],"tool_choice":"auto","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"compaction_trigger"}]}`)

	body := kimiBuildCompactionRequest(payload)

	if xaiInputHasItemType(body, "compaction_trigger") {
		t.Fatalf("compaction_trigger survived into the summarization request: %s", body)
	}
	if got := gjson.GetBytes(body, "instructions").String(); got != kimiCompactionInstructions {
		t.Fatalf("instructions = %q, want the compaction instructions", got)
	}
	if got := gjson.GetBytes(body, "tool_choice").String(); got != "none" {
		t.Fatalf("tool_choice = %q, want none", got)
	}
	if !gjson.GetBytes(body, "tools").Exists() {
		t.Fatalf("tools were dropped; the transcript still references them: %s", body)
	}
	if gjson.GetBytes(body, "text").Exists() {
		t.Fatalf("low verbosity survived into the summarization request: %s", body)
	}
	if gjson.GetBytes(body, "stream").Exists() {
		t.Fatalf("stream survived into the summarization request: %s", body)
	}

	input := gjson.GetBytes(body, "input").Array()
	if len(input) != 2 {
		t.Fatalf("input has %d items, want 2 (original message + summary request)", len(input))
	}
	last := input[len(input)-1]
	if got := last.Get("role").String(); got != "user" {
		t.Fatalf("last input role = %q, want user", got)
	}
	if got := last.Get("content.0.text").String(); got != kimiCompactionUserPrompt {
		t.Fatalf("last input text = %q, want the compaction prompt", got)
	}
}

// Codex stores the compaction item and replays it as input on every later turn.
// Kimi cannot read it, so the executor has to restore the summary it stands for.
func TestKimiCompactionItemRoundTrips(t *testing.T) {
	summary := "Objective: ship the fix.\nNext steps: run the tests."
	item := kimiCompactionOutputItem(summary, "resp_kimi_compaction_1")

	if got := gjson.GetBytes(item, "type").String(); got != "compaction" {
		t.Fatalf("item type = %q, want compaction", got)
	}
	if got := gjson.GetBytes(item, "id").String(); got != "cmp_kimi_compaction_1" {
		t.Fatalf("item id = %q, want cmp_kimi_compaction_1", got)
	}

	body := []byte(`{"model":"kimi-k3","input":[]}`)
	body, _ = sjson.SetRawBytes(body, "input.-1", item)
	body, _ = sjson.SetRawBytes(body, "input.-1", []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":"carry on"}]}`))

	expanded := kimiExpandCompactionInputItems(body)
	input := gjson.GetBytes(expanded, "input").Array()
	if len(input) != 2 {
		t.Fatalf("input has %d items, want 2", len(input))
	}
	if got := input[0].Get("type").String(); got != "message" {
		t.Fatalf("restored item type = %q, want message", got)
	}
	restored := input[0].Get("content.0.text").String()
	if !strings.Contains(restored, summary) {
		t.Fatalf("restored text lost the summary: %q", restored)
	}
	if !strings.HasPrefix(restored, kimiCompactionReplayHeader) {
		t.Fatalf("restored text is unframed: %q", restored)
	}
	if got := input[1].Get("content.0.text").String(); got != "carry on" {
		t.Fatalf("following turn was disturbed: %q", got)
	}
}

// A compaction item minted by another provider is meaningless to Kimi, and
// forwarding its encrypted_content upstream would corrupt the turn.
func TestKimiExpandCompactionInputItemsDropsForeignItems(t *testing.T) {
	body := []byte(`{"model":"kimi-k3","input":[{"type":"compaction","encrypted_content":"gAAAAABforeign"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	expanded := kimiExpandCompactionInputItems(body)

	input := gjson.GetBytes(expanded, "input").Array()
	if len(input) != 1 {
		t.Fatalf("input has %d items, want 1 after dropping the foreign compaction", len(input))
	}
	if got := input[0].Get("content.0.text").String(); got != "hi" {
		t.Fatalf("input.0 text = %q, want hi", got)
	}
}

// An ordinary turn must not be rewritten.
func TestKimiExpandCompactionInputItemsLeavesOrdinaryTurns(t *testing.T) {
	body := []byte(`{"model":"kimi-k3","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if got := string(kimiExpandCompactionInputItems(body)); got != string(body) {
		t.Fatalf("ordinary turn was rewritten: %s", got)
	}
	if got := string(kimiExpandCompactionInputItems([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))); got != `{"messages":[{"role":"user","content":"hi"}]}` {
		t.Fatalf("claude-shaped payload was rewritten: %s", got)
	}
}

// Codex fails the task unless the response carries exactly one compaction output
// item, which is what made the plain-chat-turn fallback unusable.
func TestKimiCompactionStreamCarriesExactlyOneCompactionItem(t *testing.T) {
	payload := []byte(`{"model":"kimi-k3","instructions":"You are Codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"compaction_trigger"}]}`)
	usage := []byte(`{"input_tokens":10,"output_tokens":5,"total_tokens":15}`)

	chunks := kimiBuildCompactionStreamChunks(payload, "kimi-k3", "the summary", usage)

	var stream strings.Builder
	for _, chunk := range chunks {
		stream.Write(chunk)
	}
	out := stream.String()
	for _, event := range []string{"response.created", "response.in_progress", "response.output_item.added", "response.output_item.done", "response.completed"} {
		if !strings.Contains(out, "event: "+event+"\n") {
			t.Fatalf("stream is missing %s: %s", event, out)
		}
	}

	completed := sseFramePayload(t, chunks[len(chunks)-1])
	output := gjson.GetBytes(completed, "response.output").Array()
	if len(output) != 1 {
		t.Fatalf("response.completed carries %d output items, want exactly 1", len(output))
	}
	if got := output[0].Get("type").String(); got != "compaction" {
		t.Fatalf("output item type = %q, want compaction", got)
	}
	if got := gjson.GetBytes(completed, "response.status").String(); got != "completed" {
		t.Fatalf("status = %q, want completed", got)
	}
	if got := gjson.GetBytes(completed, "response.usage.total_tokens").Int(); got != 15 {
		t.Fatalf("usage.total_tokens = %d, want 15", got)
	}
	if got := gjson.GetBytes(completed, "response.model").String(); got != "kimi-k3" {
		t.Fatalf("model = %q, want kimi-k3", got)
	}

	summary, ok := kimiCompactionSummary(output[0].Get("encrypted_content").String())
	if !ok || summary != "the summary" {
		t.Fatalf("summary = %q (ok=%v), want %q", summary, ok, "the summary")
	}
}

// Codex compares the response usage against model_context_window to decide
// whether the context is still too big. The summarization call's prompt is the
// whole transcript being compacted, so reporting it here reads as "still over the
// limit" and sends Codex into an endless compaction loop.
func TestKimiCompactionUsageReportsOnlyTheSummary(t *testing.T) {
	// A transcript that overflowed a 249k window, summarized down to 147 tokens.
	usage := kimiCompactionUsage([]byte(`{"usage":{"prompt_tokens":256724,"completion_tokens":147,"total_tokens":256871}}`))

	if got := gjson.GetBytes(usage, "total_tokens").Int(); got != 147 {
		t.Fatalf("total_tokens = %d, want 147 (the summary); the prompt must not leak into the context accounting", got)
	}
	if got := gjson.GetBytes(usage, "input_tokens").Int(); got != 0 {
		t.Fatalf("input_tokens = %d, want 0", got)
	}
	if got := gjson.GetBytes(usage, "output_tokens").Int(); got != 147 {
		t.Fatalf("output_tokens = %d, want 147", got)
	}
	if kimiCompactionUsage([]byte(`{"choices":[]}`)) != nil {
		t.Fatalf("usage was invented for a response that reported none")
	}
}

// The non-streaming answer has to satisfy the same one-item rule, and the
// /responses/compact endpoint answers with its own object type.
func TestKimiBuildCompletedCompactionResponse(t *testing.T) {
	payload := []byte(`{"model":"kimi-k3","input":[]}`)

	response := kimiBuildCompletedCompactionResponse(payload, "kimi-k3", "the summary", nil, false)
	if got := gjson.GetBytes(response, "object").String(); got != "response" {
		t.Fatalf("object = %q, want response", got)
	}
	output := gjson.GetBytes(response, "output").Array()
	if len(output) != 1 || output[0].Get("type").String() != "compaction" {
		t.Fatalf("output = %s, want exactly one compaction item", gjson.GetBytes(response, "output").Raw)
	}

	compact := kimiBuildCompletedCompactionResponse(payload, "kimi-k3", "the summary", nil, true)
	if got := gjson.GetBytes(compact, "object").String(); got != "response.compaction" {
		t.Fatalf("object = %q, want response.compaction for the compact endpoint", got)
	}
}

// Compaction is signalled two ways, and an ordinary turn is neither.
func TestKimiRequestIsCompaction(t *testing.T) {
	trigger := []byte(`{"model":"kimi-k3","input":[{"type":"message","role":"user","content":"hi"},{"type":"compaction_trigger"}]}`)
	if !kimiRequestIsCompaction(trigger, cliproxyexecutor.Options{}) {
		t.Fatalf("compaction_trigger item was not classified as compaction")
	}
	ordinary := []byte(`{"model":"kimi-k3","input":[{"type":"message","role":"user","content":"hi"}]}`)
	if !kimiRequestIsCompaction(ordinary, cliproxyexecutor.Options{Alt: "responses/compact"}) {
		t.Fatalf("the /responses/compact endpoint was not classified as compaction")
	}
	if kimiRequestIsCompaction(ordinary, cliproxyexecutor.Options{}) {
		t.Fatalf("normal turn was classified as compaction")
	}
	// Claude-format payloads have no "input" array at all.
	if kimiRequestIsCompaction([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), cliproxyexecutor.Options{}) {
		t.Fatalf("claude-shaped payload was classified as compaction")
	}
}
