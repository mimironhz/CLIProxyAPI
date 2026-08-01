package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const capturedXAICodexViewImageToolJSON = `{"type":"function","name":"view_image","description":"View a local image file from the filesystem when visual inspection is needed. Use this for images already available on disk.","strict":false,"parameters":{"type":"object","properties":{"detail":{"type":"string","description":"Image detail level. Defaults to ` + "`high`" + `; use ` + "`original`" + ` to preserve exact resolution.","enum":["high","original"]},"path":{"type":"string","description":"Local filesystem path to an image file."}},"required":["path"],"additionalProperties":false}}`

func TestPrepareXAIResponsesAliasesCapturedCodexViewImageContract(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":"inspect image","tools":[]}`)
	var errSet error
	body, errSet = sjson.SetRawBytes(body, "tools.-1", []byte(capturedXAICodexViewImageToolJSON))
	if errSet != nil {
		t.Fatalf("set captured view_image tool: %v", errSet)
	}

	exec := NewXAIExecutor(&config.Config{})
	prepared, errPrepare := exec.prepareResponsesRequest(context.Background(), cliproxyexecutor.Request{
		Model:   "grok-4.5",
		Payload: body,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex}, true)
	if errPrepare != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
	}
	if !prepared.viewImageToolAlias {
		t.Fatal("captured Codex view_image contract did not activate the alias")
	}
	if got := countXAITestTools(prepared.body, xaiReadFileToolName); got != 1 {
		t.Fatalf("prepared read_file count = %d, want 1; body=%s", got, prepared.body)
	}
	alias := findXAITestTool(prepared.body, xaiReadFileToolName)
	if got := alias.Get("parameters.required.0").String(); got != "path" {
		t.Fatalf("alias required key = %q, want path; tool=%s", got, alias.Raw)
	}
}

func TestApplyXAIViewImageToolAliasRewritesOnlyNames(t *testing.T) {
	viewImageTool := testXAICodexViewImageTool(t)
	body := mustMarshalXAITestJSON(t, map[string]any{
		"model": "grok-4.5",
		"tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
			json.RawMessage(viewImageTool),
		},
		"tool_choice": map[string]any{
			"type": "allowed_tools",
			"tools": []any{
				map[string]any{"type": "function", "name": xaiViewImageToolName},
				map[string]any{"type": "function", "name": "exec_command"},
			},
		},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "mention view_image without rewriting text"},
			map[string]any{"type": "function_call", "name": xaiViewImageToolName, "call_id": "call_image", "arguments": `{"path":"/tmp/test.png","detail":"high"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_image", "output": "ok"},
			map[string]any{"type": "function_call", "namespace": "images", "name": xaiViewImageToolName, "call_id": "call_namespaced", "arguments": `{}`},
		},
	})

	aliased, enabled := applyXAIViewImageToolAlias(body)
	if !enabled {
		t.Fatal("applyXAIViewImageToolAlias() did not enable the exact Codex contract")
	}
	aliased, ok := rewriteXAIViewImageInputCalls(aliased)
	if !ok {
		t.Fatal("rewriteXAIViewImageInputCalls() failed")
	}

	if got := gjson.GetBytes(aliased, "tools.1.name").String(); got != xaiReadFileToolName {
		t.Fatalf("tools.1.name = %q, want %q; body=%s", got, xaiReadFileToolName, aliased)
	}
	if got := gjson.GetBytes(aliased, "tool_choice.tools.0.name").String(); got != xaiReadFileToolName {
		t.Fatalf("allowed tool name = %q, want %q; body=%s", got, xaiReadFileToolName, aliased)
	}
	if got := gjson.GetBytes(aliased, "tool_choice.tools.1.name").String(); got != "exec_command" {
		t.Fatalf("unrelated allowed tool name = %q, want exec_command", got)
	}
	if got := gjson.GetBytes(aliased, "input.1.name").String(); got != xaiReadFileToolName {
		t.Fatalf("historical call name = %q, want %q", got, xaiReadFileToolName)
	}
	if got := gjson.GetBytes(aliased, "input.1.arguments").String(); got != `{"path":"/tmp/test.png","detail":"high"}` {
		t.Fatalf("historical arguments changed: %q", got)
	}
	if got := gjson.GetBytes(aliased, "input.3.name").String(); got != xaiViewImageToolName {
		t.Fatalf("namespaced historical call changed to %q", got)
	}
	if got := gjson.GetBytes(aliased, "input.0.content").String(); got != "mention view_image without rewriting text" {
		t.Fatalf("message content changed to %q", got)
	}
	if !jsonEqualXAITest(gjson.GetBytes(aliased, "tools.1.parameters").Raw, gjson.GetBytes(viewImageTool, "parameters").Raw) {
		t.Fatalf("parameters changed: %s", gjson.GetBytes(aliased, "tools.1.parameters").Raw)
	}
	if got := gjson.GetBytes(aliased, "tools.1.description").String(); got != xaiViewImageDescription {
		t.Fatalf("description changed to %q", got)
	}
}

func TestApplyXAIViewImageToolAliasRewritesForcedChoice(t *testing.T) {
	body := testXAIViewImageRequest(t, nil)
	body, errSet := sjson.SetRawBytes(body, "tool_choice", []byte(`{"type":"function","name":"view_image"}`))
	if errSet != nil {
		t.Fatalf("set tool_choice: %v", errSet)
	}
	aliased, enabled := applyXAIViewImageToolAlias(body)
	if !enabled {
		t.Fatal("alias not enabled")
	}
	if got := gjson.GetBytes(aliased, "tool_choice.name").String(); got != xaiReadFileToolName {
		t.Fatalf("tool_choice.name = %q, want %q", got, xaiReadFileToolName)
	}
}

func TestApplyXAIViewImageToolAliasCollisionGuards(t *testing.T) {
	viewImageTool := testXAICodexViewImageTool(t)
	readFileTool := mustMarshalXAITestJSON(t, map[string]any{
		"type":       "function",
		"name":       xaiReadFileToolName,
		"parameters": map[string]any{"type": "object"},
	})
	tests := map[string][]byte{
		"declared read_file": mustMarshalXAITestJSON(t, map[string]any{
			"tools": []any{json.RawMessage(viewImageTool), json.RawMessage(readFileTool)},
		}),
		"historical read_file": testXAIViewImageRequest(t, []any{
			map[string]any{"type": "function_call", "name": xaiReadFileToolName, "call_id": "old", "arguments": `{}`},
		}),
		"duplicate view_image": mustMarshalXAITestJSON(t, map[string]any{
			"tools": []any{json.RawMessage(viewImageTool), json.RawMessage(viewImageTool)},
		}),
		"different same-name contract": mustMarshalXAITestJSON(t, map[string]any{
			"tools": []any{map[string]any{"type": "function", "name": xaiViewImageToolName, "description": "different", "parameters": map[string]any{"type": "object"}}},
		}),
		"malformed JSON": []byte(`{"tools":[`),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			got, enabled := applyXAIViewImageToolAlias(body)
			if enabled {
				t.Fatal("alias unexpectedly enabled")
			}
			if !bytes.Equal(got, body) {
				t.Fatalf("guarded body changed: got=%s want=%s", got, body)
			}
		})
	}
}

func TestPrepareXAIResponsesSkipsViewImageAliasForToolSearchReadFileCollision(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","tools":[],"input":[{"type":"tool_search_output","tools":[{"type":"function","name":"read_file","description":"Client reader","parameters":{"type":"object","properties":{}}}]}]}`)
	var errSet error
	body, errSet = sjson.SetRawBytes(body, "tools.-1", []byte(capturedXAICodexViewImageToolJSON))
	if errSet != nil {
		t.Fatalf("set captured view_image tool: %v", errSet)
	}

	exec := NewXAIExecutor(&config.Config{})
	prepared, errPrepare := exec.prepareResponsesRequest(context.Background(), cliproxyexecutor.Request{
		Model:   "grok-4.5",
		Payload: body,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex}, true)
	if errPrepare != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
	}
	if prepared.viewImageToolAlias {
		t.Fatal("tool_search-loaded read_file collision unexpectedly activated alias")
	}
	if got := countXAITestTools(prepared.body, xaiViewImageToolName); got != 1 {
		t.Fatalf("prepared view_image count = %d, want 1; body=%s", got, prepared.body)
	}
	if got := countXAITestTools(prepared.body, xaiReadFileToolName); got != 1 {
		t.Fatalf("prepared read_file count = %d, want 1; body=%s", got, prepared.body)
	}
}

func TestXAIViewImageClientTranscriptRequestKeepsReplayAliasable(t *testing.T) {
	aliasedRequest := testXAIViewImageRequest(t, []any{
		map[string]any{"type": "function_call", "name": xaiReadFileToolName, "call_id": "call_image", "arguments": `{"path":"/tmp/test.png"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_image", "output": "ok"},
	})
	clientRequest := xaiViewImageClientTranscriptRequest(aliasedRequest, true)
	if got := gjson.GetBytes(clientRequest, "input.0.name").String(); got != xaiViewImageToolName {
		t.Fatalf("client transcript call name = %q, want %q", got, xaiViewImageToolName)
	}

	state := &xaiWebsocketIDState{}
	state.recordTranscriptTurn(clientRequest, []byte(`{"response":{"output":[]}}`), true)
	replayedInput := state.snapshotTranscriptInput()
	if len(replayedInput) == 0 {
		t.Fatal("recorded transcript is empty")
	}
	replayedBody := testXAIViewImageRequest(t, nil)
	var errSet error
	replayedBody, errSet = sjson.SetRawBytes(replayedBody, "input", replayedInput)
	if errSet != nil {
		t.Fatalf("set replayed input: %v", errSet)
	}
	prepared := &xaiPreparedRequest{body: replayedBody}
	applyXAIViewImageAliasToPrepared(prepared)
	if !prepared.viewImageToolAlias {
		t.Fatal("replayed client transcript disabled its own alias")
	}
	if got := gjson.GetBytes(prepared.body, "input.0.name").String(); got != xaiReadFileToolName {
		t.Fatalf("replayed upstream call name = %q, want %q", got, xaiReadFileToolName)
	}
}

func TestRestoreXAIViewImageToolAlias(t *testing.T) {
	arguments := `{"path":"/tmp/test.png","detail":"original"}`
	event := mustMarshalXAITestJSON(t, map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"tool_choice": map[string]any{"type": "function", "name": xaiReadFileToolName},
			"tools": []any{map[string]any{
				"type": "function", "name": xaiReadFileToolName,
				"description": xaiViewImageDescription,
				"parameters":  gjson.ParseBytes(testXAICodexViewImageTool(t)).Get("parameters").Value(),
			}},
			"output": []any{
				map[string]any{"type": "function_call", "name": xaiReadFileToolName, "call_id": "call_image", "arguments": arguments},
				map[string]any{"type": "function_call", "name": "exec_command", "call_id": "call_exec", "arguments": `{"cmd":"pwd"}`},
			},
		},
	})

	restored := restoreXAIViewImageToolAlias(event, true)
	if got := gjson.GetBytes(restored, "response.output.0.name").String(); got != xaiViewImageToolName {
		t.Fatalf("restored call name = %q, want %q", got, xaiViewImageToolName)
	}
	if got := gjson.GetBytes(restored, "response.output.0.arguments").String(); got != arguments {
		t.Fatalf("arguments changed: %q", got)
	}
	if got := gjson.GetBytes(restored, "response.output.1.name").String(); got != "exec_command" {
		t.Fatalf("unrelated call changed to %q", got)
	}
	if got := gjson.GetBytes(restored, "response.tools.0.name").String(); got != xaiViewImageToolName {
		t.Fatalf("echoed tool name = %q, want %q", got, xaiViewImageToolName)
	}
	if got := gjson.GetBytes(restored, "response.tool_choice.name").String(); got != xaiViewImageToolName {
		t.Fatalf("echoed tool choice = %q, want %q", got, xaiViewImageToolName)
	}
	if got := restoreXAIViewImageToolAlias(event, false); !bytes.Equal(got, event) {
		t.Fatalf("disabled restore changed event: %s", got)
	}

	added := []byte(`{"type":"response.output_item.added","item":{"type":"function_call","name":"read_file","arguments":""}}`)
	if got := gjson.GetBytes(restoreXAIViewImageToolAlias(added, true), "item.name").String(); got != xaiViewImageToolName {
		t.Fatalf("added item name = %q, want %q", got, xaiViewImageToolName)
	}
	direct := []byte(`{"output":[{"type":"function_call","name":"read_file","arguments":"{\"path\":\"/tmp/test.png\"}"}]}`)
	if got := gjson.GetBytes(restoreXAIViewImageToolAlias(direct, true), "output.0.name").String(); got != xaiViewImageToolName {
		t.Fatalf("direct output call name = %q, want %q", got, xaiViewImageToolName)
	}
	delta := []byte(`{"type":"response.function_call_arguments.delta","delta":"{\"path\":\"/tmp/test.png\"}"}`)
	if got := restoreXAIViewImageToolAlias(delta, true); !bytes.Equal(got, delta) {
		t.Fatalf("argument delta changed: got=%s want=%s", got, delta)
	}
	custom := []byte(`{"type":"response.output_item.done","item":{"type":"custom_tool_call","name":"read_file","input":"raw"}}`)
	if got := restoreXAIViewImageToolAlias(custom, true); !bytes.Equal(got, custom) {
		t.Fatalf("custom tool call changed: got=%s want=%s", got, custom)
	}
}

func TestXAIExecutorViewImageAliasRoundTripStream(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		upstreamBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request: %v", errRead)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		arguments := `{"path":"/tmp/test.png","detail":"high"}`
		_, _ = fmt.Fprintln(w, `event: response.output_item.added`)
		_, _ = fmt.Fprintln(w, `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":"","status":"in_progress"}}`)
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, `event: response.function_call_arguments.delta`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", mustMarshalXAITestJSON(t, map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_1", "delta": arguments}))
		_, _ = fmt.Fprintln(w, `event: response.output_item.done`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", mustMarshalXAITestJSON(t, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": xaiReadFileToolName, "arguments": arguments, "status": "completed"}}))
		_, _ = fmt.Fprintln(w, `event: response.completed`)
		_, _ = fmt.Fprintln(w, `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"grok-4.5","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
		_, _ = fmt.Fprintln(w)
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.5",
		Payload: testXAIViewImageRequest(t, []any{map[string]any{"type": "message", "role": "user", "content": "inspect image"}}),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var stream bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		stream.Write(chunk.Payload)
		stream.WriteByte('\n')
	}
	if got := countXAITestTools(upstreamBody, xaiReadFileToolName); got != 1 {
		t.Fatalf("upstream read_file count = %d, want 1; body=%s", got, upstreamBody)
	}
	if got := countXAITestTools(upstreamBody, xaiViewImageToolName); got != 0 {
		t.Fatalf("upstream view_image count = %d, want 0; body=%s", got, upstreamBody)
	}
	upstreamAlias := findXAITestTool(upstreamBody, xaiReadFileToolName)
	if !jsonEqualXAITest(upstreamAlias.Get("parameters").Raw, gjson.GetBytes(testXAICodexViewImageTool(t), "parameters").Raw) {
		t.Fatalf("upstream alias parameters changed: %s", upstreamAlias.Raw)
	}

	streamText := stream.String()
	if strings.Contains(streamText, `"name":"read_file"`) {
		t.Fatalf("upstream alias leaked downstream: %s", streamText)
	}
	if !strings.Contains(streamText, `"name":"view_image"`) {
		t.Fatalf("restored view_image call missing downstream: %s", streamText)
	}
	if !strings.Contains(streamText, `\"path\":\"/tmp/test.png\"`) {
		t.Fatalf("argument delta or completed arguments missing downstream: %s", streamText)
	}
}

func TestXAIExecutorViewImageAliasRoundTripNonStream(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		upstreamBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request: %v", errRead)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"/tmp/test.png\",\"detail\":\"original\"}","status":"completed"}}`)
		_, _ = fmt.Fprintln(w)
		completed := mustMarshalXAITestJSON(t, map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp_1", "object": "response", "status": "completed", "model": "grok-4.5",
				"output": []any{},
				"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
	}))
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"access_token": "xai-token"},
	}
	response, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "grok-4.5",
		Payload: testXAIViewImageRequest(t, nil),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := countXAITestTools(upstreamBody, xaiReadFileToolName); got != 1 {
		t.Fatalf("upstream read_file count = %d, want 1; body=%s", got, upstreamBody)
	}
	if strings.Contains(string(response.Payload), `"name":"read_file"`) {
		t.Fatalf("upstream alias leaked downstream: %s", response.Payload)
	}
	if !strings.Contains(string(response.Payload), `"name":"view_image"`) {
		t.Fatalf("restored view_image call missing downstream: %s", response.Payload)
	}
	if !strings.Contains(string(response.Payload), `\"path\":\"/tmp/test.png\"`) {
		t.Fatalf("arguments missing downstream: %s", response.Payload)
	}
}

func testXAICodexViewImageTool(t *testing.T) []byte {
	t.Helper()
	return mustMarshalXAITestJSON(t, map[string]any{
		"type":        xaiFunctionToolType,
		"name":        xaiViewImageToolName,
		"description": xaiViewImageDescription,
		"strict":      false,
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": xaiViewImagePathDescription},
				"detail": map[string]any{
					"type": "string", "description": xaiViewImageDetailDescription,
					"enum": []string{"high", "original"},
				},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	})
}

func testXAIViewImageRequest(t *testing.T, input []any) []byte {
	t.Helper()
	if input == nil {
		input = []any{map[string]any{"type": "message", "role": "user", "content": "inspect image"}}
	}
	return mustMarshalXAITestJSON(t, map[string]any{
		"model":               "grok-4.5",
		"input":               input,
		"tools":               []any{json.RawMessage(testXAICodexViewImageTool(t))},
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
	})
}

func mustMarshalXAITestJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		t.Fatalf("json.Marshal() error = %v", errMarshal)
	}
	return raw
}

func jsonEqualXAITest(left, right string) bool {
	var leftValue any
	var rightValue any
	if json.Unmarshal([]byte(left), &leftValue) != nil || json.Unmarshal([]byte(right), &rightValue) != nil {
		return false
	}
	leftCanonical, errLeft := json.Marshal(leftValue)
	rightCanonical, errRight := json.Marshal(rightValue)
	return errLeft == nil && errRight == nil && bytes.Equal(leftCanonical, rightCanonical)
}

func countXAITestTools(body []byte, name string) int {
	count := 0
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if tool.Get("name").String() == name {
			count++
		}
	}
	return count
}

func findXAITestTool(body []byte, name string) gjson.Result {
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if tool.Get("name").String() == name {
			return tool
		}
	}
	return gjson.Result{}
}
