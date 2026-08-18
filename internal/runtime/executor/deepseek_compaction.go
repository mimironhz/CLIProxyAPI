package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	deepSeekCompactionEncryptedContentPrefix = "deepseek-compaction-v1:"
	deepSeekCompactionReplayHeader           = "Summary of the conversation so far, standing in for the messages that were compacted away:"
	deepSeekCompactionInstructions           = `You are compacting a coding agent's conversation that has reached its context limit. Replace the transcript with one summary complete enough that the agent can continue with no other memory of it.

Cover, omitting any heading with nothing to report:
- Objective: what the user asked for, including constraints and preferences they stated.
- Work completed: what was actually done, with file paths, and the reasoning behind each decision.
- Current state: what is verified working, what is unverified, and what is known broken.
- Key facts: commands, identifiers, versions, API shapes, and other details that were expensive to discover.
- Next steps: what remains, in the order it should be taken.

Prefer specifics over narrative: exact paths, names, and numbers carry the value. Do not address the user, ask questions, or call tools. Output only the summary.`
	deepSeekCompactionUserPrompt = "Compact the conversation above as instructed. Output only the summary."
)

func (e *OpenAICompatExecutor) deepSeekRequestIsCompaction(auth *cliproxyauth.Auth, payload []byte, opts cliproxyexecutor.Options) bool {
	baseURL, _ := e.resolveCredentials(auth)
	if !helps.IsDeepSeekBaseURL(baseURL) {
		return false
	}
	return opts.Alt == "responses/compact" || xaiInputHasItemType(payload, "compaction_trigger")
}

func deepSeekBuildCompactionRequest(payload []byte) []byte {
	body := xaiRemoveInputItemsByType(payload, "compaction_trigger")
	body = deepSeekExpandCompactionInputItems(body)
	body, _ = sjson.SetBytes(body, "instructions", deepSeekCompactionInstructions)
	body, _ = sjson.SetBytes(body, "tool_choice", "none")
	body, _ = sjson.DeleteBytes(body, "text")
	body, _ = sjson.DeleteBytes(body, "stream")
	body, _ = sjson.DeleteBytes(body, "stream_options")

	message := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`)
	message, _ = sjson.SetBytes(message, "content.0.text", deepSeekCompactionUserPrompt)
	if updated, errSet := sjson.SetRawBytes(body, "input.-1", message); errSet == nil {
		body = updated
	}
	return body
}

func deepSeekExpandCompactionInputItems(body []byte) []byte {
	if !xaiInputHasItemType(body, "compaction") {
		return body
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}

	items := make([]json.RawMessage, 0, len(input.Array()))
	changed := false
	for _, item := range input.Array() {
		if item.Get("type").String() != "compaction" {
			items = append(items, json.RawMessage(item.Raw))
			continue
		}
		changed = true
		summary, ok := deepSeekCompactionSummary(item.Get("encrypted_content").String())
		if !ok {
			continue
		}
		message := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`)
		message, _ = sjson.SetBytes(message, "content.0.text", deepSeekCompactionReplayHeader+"\n\n"+summary)
		items = append(items, json.RawMessage(message))
	}
	if !changed {
		return body
	}

	encoded, errMarshal := json.Marshal(items)
	if errMarshal != nil {
		return body
	}
	updated, errSet := sjson.SetRawBytes(body, "input", encoded)
	if errSet != nil {
		return body
	}
	return updated
}

func deepSeekCompactionSummary(encryptedContent string) (string, bool) {
	if !strings.HasPrefix(encryptedContent, deepSeekCompactionEncryptedContentPrefix) {
		return "", false
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(strings.TrimPrefix(encryptedContent, deepSeekCompactionEncryptedContentPrefix))
	if errDecode != nil {
		return "", false
	}
	summary := strings.TrimSpace(string(decoded))
	return summary, summary != ""
}

func (e *OpenAICompatExecutor) executeDeepSeekCompaction(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (summary string, usage []byte, headers http.Header, err error) {
	requestKind := "websocket_trigger"
	if opts.Alt == "responses/compact" {
		requestKind = "responses_compact"
	}
	helpFields := helps.CompactionDiagnosticFields(req.Payload)
	helpFields["compaction_provider"] = "deepseek"
	helpFields["compaction_phase"] = "client_request"
	helpFields["request_kind"] = requestKind
	helps.LogWithRequestID(ctx).WithFields(helpFields).Info("compaction capture")

	compactReq := req
	compactReq.Payload = deepSeekBuildCompactionRequest(bytes.Clone(req.Payload))
	compactOpts := opts
	compactOpts.Alt = ""
	compactOpts.Stream = false
	compactOpts.OriginalRequest = nil

	response, errExecute := e.Execute(ctx, auth, compactReq, compactOpts)
	if errExecute != nil {
		return "", nil, nil, errExecute
	}
	summary = deepSeekResponsesMessageText(response.Payload)
	if summary == "" {
		return "", nil, nil, statusErr{code: http.StatusBadGateway, msg: "deepseek compaction produced an empty summary"}
	}

	responseFields := helps.CompactionDiagnosticFields(response.Payload)
	responseFields["compaction_provider"] = "deepseek"
	responseFields["compaction_phase"] = "summarizer_response"
	responseFields["request_kind"] = requestKind
	helpLog := helps.LogWithRequestID(ctx).WithFields(responseFields)
	if requestID := strings.TrimSpace(response.Headers.Get("x-request-id")); requestID != "" {
		helpLog = helpLog.WithField("upstream_request_id", requestID)
	}
	helpLog.Info("compaction capture")

	return summary, deepSeekCompactionUsage(summary, req.Model), response.Headers.Clone(), nil
}

func deepSeekResponsesMessageText(body []byte) string {
	output := gjson.GetBytes(body, "output")
	if !output.IsArray() {
		return ""
	}
	texts := make([]string, 0, 4)
	for _, item := range output.Array() {
		if item.Get("type").String() != "message" || item.Get("role").String() != "assistant" {
			continue
		}
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		for _, part := range content.Array() {
			if part.Get("type").String() != "output_text" {
				continue
			}
			if text := strings.TrimSpace(part.Get("text").String()); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

func deepSeekCompactionUsage(summary, model string) []byte {
	count := 0
	if codec, errCodec := helps.TokenizerForModel(model); errCodec == nil {
		if counted, errCount := codec.Count(summary); errCount == nil {
			count = counted
		}
	}
	if count <= 0 && summary != "" {
		count = max(1, len(summary)/4)
	}
	usage := []byte(`{"input_tokens":0,"output_tokens":0,"total_tokens":0}`)
	usage, _ = sjson.SetBytes(usage, "output_tokens", count)
	usage, _ = sjson.SetBytes(usage, "total_tokens", count)
	return usage
}

func deepSeekCompactionOutputItem(summary, responseID string) []byte {
	item := []byte(`{"type":"compaction","encrypted_content":""}`)
	item, _ = sjson.SetBytes(item, "id", "cmp_"+strings.TrimPrefix(responseID, "resp_"))
	item, _ = sjson.SetBytes(item, "encrypted_content", deepSeekCompactionEncryptedContentPrefix+base64.StdEncoding.EncodeToString([]byte(summary)))
	return item
}

func deepSeekCompactionResponse(payload []byte, model, responseID string, createdAt int64, status string) []byte {
	response := []byte(`{"id":"","object":"response","created_at":0,"status":"","background":false,"error":null,"incomplete_details":null,"output":[]}`)
	response, _ = sjson.SetBytes(response, "id", responseID)
	response, _ = sjson.SetBytes(response, "created_at", createdAt)
	response, _ = sjson.SetBytes(response, "status", status)
	if requestModel := strings.TrimSpace(gjson.GetBytes(payload, "model").String()); requestModel != "" {
		model = requestModel
	}
	if model != "" {
		response, _ = sjson.SetBytes(response, "model", model)
	}
	for _, field := range []string{
		"instructions",
		"max_output_tokens",
		"max_tool_calls",
		"parallel_tool_calls",
		"previous_response_id",
		"prompt_cache_key",
		"reasoning",
		"text",
		"tool_choice",
		"tools",
		"truncation",
		"user",
		"metadata",
	} {
		if value := gjson.GetBytes(payload, field); value.Exists() {
			response, _ = sjson.SetRawBytes(response, field, []byte(value.Raw))
		}
	}
	return response
}

func deepSeekBuildCompletedCompactionResponse(payload []byte, model, summary string, usage []byte, compactEndpoint bool) []byte {
	responseID := fmt.Sprintf("resp_deepseek_compaction_%d", time.Now().UnixNano())
	now := time.Now().Unix()
	response := deepSeekCompactionResponse(payload, model, responseID, now, "completed")
	response, _ = sjson.SetBytes(response, "completed_at", now)
	response, _ = sjson.SetRawBytes(response, "output", deepSeekCompactionOutputArray(deepSeekCompactionOutputItem(summary, responseID)))
	if len(usage) > 0 {
		response, _ = sjson.SetRawBytes(response, "usage", usage)
	}
	if compactEndpoint {
		response, _ = sjson.SetBytes(response, "object", "response.compaction")
	}
	return response
}

func deepSeekBuildCompactionStreamChunks(payload []byte, model, summary string, usage []byte) [][]byte {
	responseID := fmt.Sprintf("resp_deepseek_compaction_%d", time.Now().UnixNano())
	now := time.Now().Unix()
	item := deepSeekCompactionOutputItem(summary, responseID)

	createdResponse := deepSeekCompactionResponse(payload, model, responseID, now, "in_progress")
	inProgressResponse := deepSeekCompactionResponse(payload, model, responseID, now, "in_progress")
	completedResponse := deepSeekCompactionResponse(payload, model, responseID, now, "completed")
	completedResponse, _ = sjson.SetBytes(completedResponse, "completed_at", now)
	completedResponse, _ = sjson.SetRawBytes(completedResponse, "output", deepSeekCompactionOutputArray(item))
	if len(usage) > 0 {
		completedResponse, _ = sjson.SetRawBytes(completedResponse, "usage", usage)
	}

	createdPayload := []byte(`{"type":"response.created","sequence_number":0}`)
	createdPayload, _ = sjson.SetRawBytes(createdPayload, "response", createdResponse)
	inProgressPayload := []byte(`{"type":"response.in_progress","sequence_number":1}`)
	inProgressPayload, _ = sjson.SetRawBytes(inProgressPayload, "response", inProgressResponse)
	addedPayload := []byte(`{"type":"response.output_item.added","sequence_number":2,"output_index":0}`)
	addedPayload, _ = sjson.SetRawBytes(addedPayload, "item", item)
	donePayload := []byte(`{"type":"response.output_item.done","sequence_number":3,"output_index":0}`)
	donePayload, _ = sjson.SetRawBytes(donePayload, "item", item)
	completedPayload := []byte(`{"type":"response.completed","sequence_number":4}`)
	completedPayload, _ = sjson.SetRawBytes(completedPayload, "response", completedResponse)

	return [][]byte{
		xaiBuildSSEFrame("response.created", createdPayload),
		xaiBuildSSEFrame("response.in_progress", inProgressPayload),
		xaiBuildSSEFrame("response.output_item.added", addedPayload),
		xaiBuildSSEFrame("response.output_item.done", donePayload),
		xaiBuildSSEFrame("response.completed", completedPayload),
	}
}

func deepSeekCompactionOutputArray(item []byte) []byte {
	output := make([]byte, 0, len(item)+2)
	output = append(output, '[')
	output = append(output, item...)
	output = append(output, ']')
	return output
}

func (e *OpenAICompatExecutor) executeDeepSeekCompactionResponse(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	summary, usage, headers, errCompact := e.executeDeepSeekCompaction(ctx, auth, req, opts)
	if errCompact != nil {
		return cliproxyexecutor.Response{}, errCompact
	}
	out := deepSeekBuildCompletedCompactionResponse(req.Payload, req.Model, summary, usage, opts.Alt == "responses/compact")
	helpFields := helps.CompactionDiagnosticFields(out)
	helpFields["compaction_provider"] = "deepseek"
	helpFields["compaction_phase"] = "client_response"
	helpFields["request_kind"] = "responses_compact"
	helpLog := helps.LogWithRequestID(ctx).WithFields(helpFields)
	helpLog.Info("compaction capture")
	return cliproxyexecutor.Response{Payload: out, Headers: headers}, nil
}

func (e *OpenAICompatExecutor) executeDeepSeekCompactionStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	summary, usage, headers, errCompact := e.executeDeepSeekCompaction(ctx, auth, req, opts)
	if errCompact != nil {
		return nil, errCompact
	}
	if headers == nil {
		headers = make(http.Header)
	} else {
		headers = headers.Clone()
	}
	headers.Set("Content-Type", "text/event-stream")
	chunks := deepSeekBuildCompactionStreamChunks(req.Payload, req.Model, summary, usage)
	helpFields := helps.CompactionStreamDiagnosticFields(chunks)
	helpFields["compaction_provider"] = "deepseek"
	helpFields["compaction_phase"] = "client_response"
	helpFields["request_kind"] = "websocket_trigger"
	helpLog := helps.LogWithRequestID(ctx).WithFields(helpFields)
	helpLog.Info("compaction capture")
	out := make(chan cliproxyexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(out)
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}
