package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	kimiauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kimi"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const kimiReasoningUnavailable = "[reasoning unavailable]"

// KimiExecutor is a stateless executor for Kimi API using OpenAI-compatible chat completions.
type KimiExecutor struct {
	ClaudeExecutor
	cfg *config.Config
}

// NewKimiExecutor creates a new Kimi executor.
func NewKimiExecutor(cfg *config.Config) *KimiExecutor {
	return &KimiExecutor{
		ClaudeExecutor: ClaudeExecutor{
			cfg:                     cfg,
			requestLogProvider:      "kimi",
			upstreamModelNormalizer: normalizeKimiUpstreamModel,
		},
		cfg: cfg,
	}
}

// Identifier returns the executor identifier.
func (e *KimiExecutor) Identifier() string { return "kimi" }

// RequestToFormat reports the upstream request format used after auth selection.
func (e *KimiExecutor) RequestToFormat(_ cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format {
	if opts.SourceFormat == sdktranslator.FormatClaude {
		return sdktranslator.FormatClaude
	}
	return sdktranslator.FormatOpenAI
}

// PrepareRequest injects Kimi credentials into the outgoing HTTP request.
func (e *KimiExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token := kimiCreds(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Kimi credentials into the request and executes it.
func (e *KimiExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kimi executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// kimiRequestIsCompaction reports whether a request asks for context compaction.
//
// Kimi For Coding exposes only an OpenAI-compatible Chat Completions API, so
// there is no compaction endpoint to forward to. The executor stands in for one:
// it summarizes the transcript with an ordinary chat turn and returns the single
// compaction output item Codex requires. Served as a plain turn instead, Codex
// fails the task with "expected exactly one compaction output item, got 0 from N
// output items"; answered with an error, Codex retries the compact task without
// bound and the thread cannot get past its context limit.
//
// Clients signal compaction two different ways, and both must be caught:
//   - HTTP clients POST to /responses/compact, surfaced as opts.Alt.
//   - Codex Desktop runs over the Responses websocket, where every turn is a
//     response.create and compaction is instead marked by a compaction_trigger
//     input item. opts.Alt is empty there.
//
// The xAI helpers reused below (xaiInputHasItemType, xaiRemoveInputItemsByType,
// xaiBuildSSEFrame) are provider-agnostic operations on the Responses shape.
func kimiRequestIsCompaction(payload []byte, opts cliproxyexecutor.Options) bool {
	if opts.Alt == "responses/compact" {
		return true
	}
	return xaiInputHasItemType(payload, "compaction_trigger")
}

// kimiCompactionEncryptedContentPrefix marks a compaction item this executor
// synthesized. A real provider returns an opaque blob it can decrypt on later
// turns; Kimi has no such facility, so the summary travels base64-encoded in the
// same field. The field is opaque to the client either way, and only this
// executor reads it back.
const kimiCompactionEncryptedContentPrefix = "kimi-compaction-v1:"

// kimiCompactionInstructions replaces the agent instructions for the single
// summarization turn that stands in for a compaction endpoint.
const kimiCompactionInstructions = `You are compacting a coding agent's conversation that has reached its context limit. Replace the transcript with one summary complete enough that the agent can continue with no other memory of it.

Cover, omitting any heading with nothing to report:
- Objective: what the user asked for, including constraints and preferences they stated.
- Work completed: what was actually done, with file paths, and the reasoning behind each decision.
- Current state: what is verified working, what is unverified, and what is known broken.
- Key facts: commands, identifiers, versions, API shapes, and other details that were expensive to discover.
- Next steps: what remains, in the order it should be taken.

Prefer specifics over narrative: exact paths, names, and numbers carry the value. Do not address the user, ask questions, or call tools. Output only the summary.`

// kimiCompactionUserPrompt is the final turn that asks for the summary.
const kimiCompactionUserPrompt = "Compact the conversation above as instructed. Output only the summary."

// kimiCompactionReplayHeader frames a restored summary for the next turn.
const kimiCompactionReplayHeader = "Summary of the conversation so far, standing in for the messages that were compacted away:"

// kimiBuildCompactionRequest turns a compaction request into an ordinary
// summarization turn: the trigger is removed, the agent instructions are
// replaced, and a final user turn asks for the summary. Tools are kept because
// the transcript still contains calls that reference them, but tool_choice is
// pinned to "none" so the answer comes back as text.
func kimiBuildCompactionRequest(payload []byte) []byte {
	body := xaiRemoveInputItemsByType(payload, "compaction_trigger")
	body = kimiExpandCompactionInputItems(body)
	body, _ = sjson.SetBytes(body, "instructions", kimiCompactionInstructions)
	body, _ = sjson.SetBytes(body, "tool_choice", "none")
	// Codex asks for low verbosity; a compaction summary needs the opposite.
	body, _ = sjson.DeleteBytes(body, "text")
	body, _ = sjson.DeleteBytes(body, "stream")
	body, _ = sjson.DeleteBytes(body, "stream_options")

	message := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`)
	message, _ = sjson.SetBytes(message, "content.0.text", kimiCompactionUserPrompt)
	if updated, err := sjson.SetRawBytes(body, "input.-1", message); err == nil {
		body = updated
	}
	return body
}

// kimiExpandCompactionInputItems restores summaries this executor produced.
// Codex replays the compaction item as ordinary input on every later turn, and
// Kimi cannot read it, so each one becomes a plain user message. Items minted by
// another provider are dropped: their encrypted_content means nothing here, and
// forwarding it upstream would only corrupt the turn.
func kimiExpandCompactionInputItems(body []byte) []byte {
	// Every ordinary turn passes through here, so leave without allocating when
	// there is nothing to restore.
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
		summary, ok := kimiCompactionSummary(item.Get("encrypted_content").String())
		if !ok {
			continue
		}
		message := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`)
		message, _ = sjson.SetBytes(message, "content.0.text", kimiCompactionReplayHeader+"\n\n"+summary)
		items = append(items, json.RawMessage(message))
	}
	if !changed {
		return body
	}

	encoded, err := json.Marshal(items)
	if err != nil {
		return body
	}
	updated, err := sjson.SetRawBytes(body, "input", encoded)
	if err != nil {
		return body
	}
	return updated
}

// kimiCompactionSummary decodes a summary this executor previously encoded.
func kimiCompactionSummary(encryptedContent string) (string, bool) {
	if !strings.HasPrefix(encryptedContent, kimiCompactionEncryptedContentPrefix) {
		return "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(encryptedContent, kimiCompactionEncryptedContentPrefix))
	if err != nil {
		return "", false
	}
	summary := strings.TrimSpace(string(decoded))
	if summary == "" {
		return "", false
	}
	return summary, true
}

// kimiCompactionOutputItem builds the one output item Codex requires.
func kimiCompactionOutputItem(summary, responseID string) []byte {
	item := []byte(`{"type":"compaction","encrypted_content":""}`)
	item, _ = sjson.SetBytes(item, "id", "cmp_"+strings.TrimPrefix(responseID, "resp_"))
	item, _ = sjson.SetBytes(item, "encrypted_content", kimiCompactionEncryptedContentPrefix+base64.StdEncoding.EncodeToString([]byte(summary)))
	return item
}

// kimiBuildCompactionResponse echoes the request configuration back the way the
// Responses API does, so the synthesized response is indistinguishable from an
// upstream one apart from its output.
func kimiBuildCompactionResponse(payload []byte, model, responseID string, createdAt int64, status string) []byte {
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

// kimiBuildCompletedCompactionResponse returns the terminal compaction response.
// The /responses/compact endpoint answers with object "response.compaction",
// so the Alt path keeps that shape.
func kimiBuildCompletedCompactionResponse(payload []byte, model, summary string, usage []byte, compactEndpoint bool) []byte {
	responseID := kimiCompactionResponseID()
	now := time.Now().Unix()
	response := kimiBuildCompactionResponse(payload, model, responseID, now, "completed")
	response, _ = sjson.SetBytes(response, "completed_at", now)
	response, _ = sjson.SetRawBytes(response, "output", kimiCompactionOutputArray(kimiCompactionOutputItem(summary, responseID)))
	if len(usage) > 0 {
		response, _ = sjson.SetRawBytes(response, "usage", usage)
	}
	if compactEndpoint {
		response, _ = sjson.SetBytes(response, "object", "response.compaction")
	}
	return response
}

// kimiBuildCompactionStreamChunks replays the synthesized response as the SSE
// frames a streaming Responses call would have produced.
func kimiBuildCompactionStreamChunks(payload []byte, model, summary string, usage []byte) [][]byte {
	responseID := kimiCompactionResponseID()
	now := time.Now().Unix()
	item := kimiCompactionOutputItem(summary, responseID)

	createdResponse := kimiBuildCompactionResponse(payload, model, responseID, now, "in_progress")
	inProgressResponse := kimiBuildCompactionResponse(payload, model, responseID, now, "in_progress")
	completedResponse := kimiBuildCompactionResponse(payload, model, responseID, now, "completed")
	completedResponse, _ = sjson.SetBytes(completedResponse, "completed_at", now)
	completedResponse, _ = sjson.SetRawBytes(completedResponse, "output", kimiCompactionOutputArray(item))
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

func kimiCompactionOutputArray(item []byte) []byte {
	output := make([]byte, 0, len(item)+2)
	output = append(output, '[')
	output = append(output, item...)
	output = append(output, ']')
	return output
}

func kimiCompactionResponseID() string {
	return fmt.Sprintf("resp_kimi_compaction_%d", time.Now().UnixNano())
}

// kimiCompactionUsage reports the size of the compaction result, not the cost of
// producing it.
//
// The summarization call's prompt is the whole transcript being compacted, so its
// prompt_tokens sits at or above the context window by construction. Codex reads
// this field as the conversation's current size and compares it against
// model_context_window: report the prompt there and every compaction ends with
// "still over the limit", which triggers another compaction, forever. The
// transcript grows each round, so the reported figure climbs with it.
//
// Only the summary survives into the next turn, so only the summary is reported.
// The true cost is still recorded through the executor's usage reporter.
func kimiCompactionUsage(data []byte) []byte {
	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() {
		return nil
	}
	summaryTokens := usage.Get("completion_tokens").Int()
	out := []byte(`{"input_tokens":0,"output_tokens":0,"total_tokens":0}`)
	out, _ = sjson.SetBytes(out, "output_tokens", summaryTokens)
	out, _ = sjson.SetBytes(out, "total_tokens", summaryTokens)
	return out
}

// executeCompaction runs the summarization turn that stands in for a compaction
// endpoint and returns the summary text.
func (e *KimiExecutor) executeCompaction(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (summary string, usage []byte, headers http.Header, err error) {
	from := opts.SourceFormat
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	token := kimiCreds(auth)
	requestKind := "websocket_trigger"
	if opts.Alt == "responses/compact" {
		requestKind = "responses_compact"
	}
	helps.LogWithRequestID(ctx).WithFields(helps.CompactionDiagnosticFields(req.Payload)).WithFields(log.Fields{
		"compaction_provider": "kimi",
		"compaction_phase":    "client_request",
		"request_kind":        requestKind,
	}).Info("compaction capture")

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	compactPayload := helps.PrepareResponsesToolSearch(kimiBuildCompactionRequest(bytes.Clone(req.Payload)))
	body := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, compactPayload, false)

	body, err = sjson.SetBytes(body, "model", normalizeKimiUpstreamModel(baseModel))
	if err != nil {
		return "", nil, nil, fmt.Errorf("kimi executor: failed to set model in compaction payload: %w", err)
	}
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "kimi", e.Identifier())
	if err != nil {
		return "", nil, nil, err
	}
	body, err = normalizeKimiToolMessageLinks(body)
	if err != nil {
		return "", nil, nil, err
	}
	reporter.SetTranslatedReasoningEffort(body, e.Identifier())

	url := kimiauth.KimiAPIBaseURL + "/v1/chat/completions"
	helps.LogWithRequestID(ctx).WithFields(helps.CompactionDiagnosticFields(body)).WithFields(log.Fields{
		"compaction_provider": "kimi",
		"compaction_phase":    "upstream_request",
		"request_kind":        requestKind,
		"upstream_url":        url,
	}).Info("compaction capture")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", nil, nil, err
	}
	applyKimiHeadersWithAuth(httpReq, token, false, auth)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		helps.LogWithRequestID(ctx).WithError(err).WithFields(log.Fields{
			"compaction_provider": "kimi",
			"compaction_phase":    "upstream_transport_error",
			"request_kind":        requestKind,
			"upstream_url":        url,
		}).Warn("compaction capture")
		return "", nil, nil, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		helps.LogWithRequestID(ctx).WithError(err).WithFields(log.Fields{
			"compaction_provider": "kimi",
			"compaction_phase":    "upstream_read_error",
			"http_status":         httpResp.StatusCode,
			"request_kind":        requestKind,
			"upstream_url":        url,
		}).Warn("compaction capture")
		return "", nil, nil, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	responseFields := helps.CompactionDiagnosticFields(data)
	responseFields["compaction_provider"] = "kimi"
	responseFields["compaction_phase"] = "upstream_response"
	responseFields["http_status"] = httpResp.StatusCode
	responseFields["request_kind"] = requestKind
	responseFields["upstream_url"] = url
	if upstreamRequestID := strings.TrimSpace(httpResp.Header.Get("x-request-id")); upstreamRequestID != "" {
		responseFields["upstream_request_id"] = upstreamRequestID
	}
	helps.LogWithRequestID(ctx).WithFields(responseFields).Info("compaction capture")

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("compaction request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = statusErr{code: httpResp.StatusCode, msg: string(data)}
		return "", nil, nil, err
	}

	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	summary = strings.TrimSpace(gjson.GetBytes(data, "choices.0.message.content").String())
	if summary == "" {
		err = statusErr{code: http.StatusBadGateway, msg: "kimi compaction produced an empty summary"}
		return "", nil, nil, err
	}
	return summary, kimiCompactionUsage(data), httpResp.Header.Clone(), nil
}

// Execute performs a non-streaming chat completion request to Kimi.
func (e *KimiExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if kimiRequestIsCompaction(req.Payload, opts) {
		summary, usage, headers, errCompact := e.executeCompaction(ctx, auth, req, opts)
		if errCompact != nil {
			return resp, errCompact
		}
		out := kimiBuildCompletedCompactionResponse(req.Payload, req.Model, summary, usage, opts.Alt == "responses/compact")
		requestKind := "websocket_trigger"
		if opts.Alt == "responses/compact" {
			requestKind = "responses_compact"
		}
		helps.LogWithRequestID(ctx).WithFields(helps.CompactionDiagnosticFields(out)).WithFields(log.Fields{
			"compaction_provider": "kimi",
			"compaction_phase":    "client_response",
			"request_kind":        requestKind,
		}).Info("compaction capture")
		return cliproxyexecutor.Response{Payload: out, Headers: headers}, nil
	}
	from := opts.SourceFormat
	if from.String() == "claude" {
		auth.Attributes["base_url"] = kimiauth.KimiAPIBaseURL
		preparedReq, replayScope := prepareKimiThinkingReplayRequest(ctx, req, opts)
		claudeResp, errExecute := e.ClaudeExecutor.Execute(ctx, auth, preparedReq, opts)
		if errExecute != nil {
			if replayScope.replayApplied && shouldClearKimiThinkingReplayAfterError(errExecute) {
				clearKimiThinkingReplayContent(ctx, replayScope)
			}
			return claudeResp, errExecute
		}
		cacheKimiThinkingReplayResponse(ctx, replayScope, claudeResp.Payload)
		return claudeResp, nil
	}
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)

	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token := kimiCreds(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	// Codex replays the compaction item on every turn after a compaction; restore
	// the summary it stands for before the payload is translated.
	originalPayload := helps.PrepareResponsesToolSearch(kimiExpandCompactionInputItems(bytes.Clone(originalPayloadSource)))
	originalTranslated := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, false)
	body := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, helps.PrepareResponsesToolSearch(kimiExpandCompactionInputItems(bytes.Clone(req.Payload))), false)

	// Strip kimi- prefix and any [1m] suffix for upstream API
	upstreamModel := normalizeKimiUpstreamModel(baseModel)
	body, err = sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return resp, fmt.Errorf("kimi executor: failed to set model in payload: %w", err)
	}

	body, err = helps.ApplyThinkingWithSourcePayload(body, req.Payload, originalPayloadSource, req.Model, from.String(), "kimi", e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body, err = normalizeKimiToolMessageLinks(body)
	if err != nil {
		return resp, err
	}
	reporter.SetTranslatedReasoningEffort(body, e.Identifier())

	url := kimiauth.KimiAPIBaseURL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	applyKimiHeadersWithAuth(httpReq, token, false, auth)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	var param any
	// Note: TranslateNonStream uses req.Model (original with suffix) to preserve
	// the original model name in the response for client compatibility.
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, data, &param)
	// Kimi calls the tool_search stand-in as a plain function; Codex Desktop only
	// runs its deferred-tool loader for a real tool_search_call item.
	out = helps.RestoreToolSearchResponse(out)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming chat completion request to Kimi.
func (e *KimiExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if kimiRequestIsCompaction(req.Payload, opts) {
		summary, usage, headers, errCompact := e.executeCompaction(ctx, auth, req, opts)
		if errCompact != nil {
			return nil, errCompact
		}
		if headers == nil {
			headers = make(http.Header)
		} else {
			headers = headers.Clone()
		}
		headers.Set("Content-Type", "text/event-stream")
		chunks := kimiBuildCompactionStreamChunks(req.Payload, req.Model, summary, usage)
		helps.LogWithRequestID(ctx).WithFields(helps.CompactionStreamDiagnosticFields(chunks)).WithFields(log.Fields{
			"compaction_provider": "kimi",
			"compaction_phase":    "client_response",
			"request_kind":        "websocket_trigger",
		}).Info("compaction capture")
		out := make(chan cliproxyexecutor.StreamChunk, len(chunks))
		for _, chunk := range chunks {
			out <- cliproxyexecutor.StreamChunk{Payload: chunk}
		}
		close(out)
		return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
	}
	from := opts.SourceFormat
	if from.String() == "claude" {
		auth.Attributes["base_url"] = kimiauth.KimiAPIBaseURL
		preparedReq, replayScope := prepareKimiThinkingReplayRequest(ctx, req, opts)
		claudeResult, errExecute := e.ClaudeExecutor.ExecuteStream(ctx, auth, preparedReq, opts)
		if errExecute != nil {
			if replayScope.replayApplied && shouldClearKimiThinkingReplayAfterError(errExecute) {
				clearKimiThinkingReplayContent(ctx, replayScope)
			}
			return nil, errExecute
		}
		return wrapKimiThinkingReplayStream(ctx, claudeResult, replayScope), nil
	}
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	token := kimiCreds(auth)

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	// Codex replays the compaction item on every turn after a compaction; restore
	// the summary it stands for before the payload is translated.
	originalPayload := helps.PrepareResponsesToolSearch(kimiExpandCompactionInputItems(bytes.Clone(originalPayloadSource)))
	originalTranslated := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, true)
	body := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, helps.PrepareResponsesToolSearch(kimiExpandCompactionInputItems(bytes.Clone(req.Payload))), true)

	// Strip kimi- prefix and any [1m] suffix for upstream API
	upstreamModel := normalizeKimiUpstreamModel(baseModel)
	body, err = sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return nil, fmt.Errorf("kimi executor: failed to set model in payload: %w", err)
	}

	body, err = helps.ApplyThinkingWithSourcePayload(body, req.Payload, originalPayloadSource, req.Model, from.String(), "kimi", e.Identifier())
	if err != nil {
		return nil, err
	}

	body, err = sjson.SetBytes(body, "stream_options.include_usage", true)
	if err != nil {
		return nil, fmt.Errorf("kimi executor: failed to set stream_options in payload: %w", err)
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body, err = normalizeKimiToolMessageLinks(body)
	if err != nil {
		return nil, err
	}
	reporter.SetTranslatedReasoningEffort(body, e.Identifier())

	url := kimiauth.KimiAPIBaseURL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyKimiHeadersWithAuth(httpReq, token, true, auth)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kimi executor: close response body error: %v", errClose)
		}
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("kimi executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 1_048_576) // 1MB
		claudeInputTokens := helps.NewClaudeInputTokenState(from, to, responseFormat, originalPayload)
		var param any
		var streamUsage helps.StreamUsageBuffer
		defer streamUsage.Publish(ctx, reporter)
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			streamUsage.ObserveOpenAIStream(line)
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param, claudeInputTokens)
			for i := range chunks {
				// Kimi calls the tool_search stand-in as a plain function; Codex Desktop
				// only runs its deferred-tool loader for a real tool_search_call item.
				chunks[i] = helps.RestoreToolSearchStreamChunk(chunks[i])
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		doneChunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, body, []byte("[DONE]"), &param, claudeInputTokens)
		for i := range doneChunks {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: doneChunks[i]}:
			case <-ctx.Done():
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens estimates token count for Kimi requests.
func (e *KimiExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	auth.Attributes["base_url"] = kimiauth.KimiAPIBaseURL
	return e.ClaudeExecutor.countTokensUpstream(ctx, auth, req, opts)
}

func normalizeKimiToolMessageLinks(body []byte) ([]byte, error) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, nil
	}

	messages := util.GetGJSONBytesNoCopy(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return body, nil
	}

	type messagePatch struct {
		index        int
		path         string
		value        string
		errorContext string
	}

	msgs := messages.Array()
	droppedMessages := make([]bool, len(msgs))
	patches := make([]messagePatch, 0)
	pending := make([]string, 0)
	dropped := 0
	patched := 0
	patchedReasoning := 0
	ambiguous := 0
	latestReasoning := ""
	hasLatestReasoning := false

	removePending := func(id string) {
		for idx := range pending {
			if pending[idx] != id {
				continue
			}
			pending = append(pending[:idx], pending[idx+1:]...)
			return
		}
	}

	for msgIndex, msg := range msgs {
		if shouldDropKimiAssistantMessage(msg) {
			droppedMessages[msgIndex] = true
			dropped++
			continue
		}

		role := strings.TrimSpace(msg.Get("role").String())
		switch role {
		case "assistant":
			reasoning := msg.Get("reasoning_content")
			if reasoning.Exists() {
				reasoningText := reasoning.String()
				if isUsableKimiReasoning(reasoningText) {
					latestReasoning = reasoningText
					hasLatestReasoning = true
				}
			}

			toolCalls := msg.Get("tool_calls")
			if toolCalls.Exists() && toolCalls.IsArray() {
				toolCallItems := toolCalls.Array()
				if len(toolCallItems) > 0 {
					if !reasoning.Exists() || !isUsableKimiReasoning(reasoning.String()) {
						patches = append(patches, messagePatch{
							index:        msgIndex,
							path:         "reasoning_content",
							value:        fallbackAssistantReasoning(msg, hasLatestReasoning, latestReasoning),
							errorContext: "failed to set assistant reasoning_content",
						})
						patchedReasoning++
					}
					for _, toolCall := range toolCallItems {
						id := strings.TrimSpace(toolCall.Get("id").String())
						if id != "" {
							pending = append(pending, id)
						}
					}
				}
			}
		case "tool":
			toolCallID := strings.TrimSpace(msg.Get("tool_call_id").String())
			if toolCallID == "" {
				toolCallID = strings.TrimSpace(msg.Get("call_id").String())
				if toolCallID != "" {
					patches = append(patches, messagePatch{index: msgIndex, path: "tool_call_id", value: toolCallID, errorContext: "failed to set tool_call_id from call_id"})
					patched++
				}
			}
			if toolCallID == "" {
				if len(pending) == 1 {
					toolCallID = pending[0]
					patches = append(patches, messagePatch{index: msgIndex, path: "tool_call_id", value: toolCallID, errorContext: "failed to infer tool_call_id"})
					patched++
				} else if len(pending) > 1 {
					ambiguous++
				}
			}
			if toolCallID != "" {
				removePending(toolCallID)
			}
		}
	}

	if dropped > 0 {
		log.WithField("dropped_assistant_messages", dropped).Debug("kimi executor: dropped empty assistant messages")
	}
	if dropped == 0 && len(patches) == 0 {
		if ambiguous > 0 {
			log.WithFields(log.Fields{
				"ambiguous_tool_messages": ambiguous,
				"pending_tool_calls":      len(pending),
			}).Warn("kimi executor: tool messages missing tool_call_id with ambiguous candidates")
		}
		return body, nil
	}

	var out []byte
	if dropped == 0 && len(patches) == 1 {
		patch := patches[0]
		path := fmt.Sprintf("messages.%d.%s", patch.index, patch.path)
		updated, errSet := sjson.SetBytes(body, path, patch.value)
		if errSet != nil {
			return body, fmt.Errorf("kimi executor: %s: %w", patch.errorContext, errSet)
		}
		out = updated
	} else {
		messageItems := make([]string, 0, len(msgs)-dropped)
		patchIndex := 0
		for msgIndex, msg := range msgs {
			if droppedMessages[msgIndex] {
				continue
			}
			messageJSON := msg.Raw
			for patchIndex < len(patches) && patches[patchIndex].index == msgIndex {
				patch := patches[patchIndex]
				next, errSet := sjson.SetBytes([]byte(messageJSON), patch.path, patch.value)
				if errSet != nil {
					return body, fmt.Errorf("kimi executor: %s: %w", patch.errorContext, errSet)
				}
				messageJSON = string(next)
				patchIndex++
			}
			messageItems = append(messageItems, messageJSON)
		}
		updated, errSet := sjson.SetRawBytes(body, "messages", helps.JoinRawJSONStrings(messageItems))
		if errSet != nil {
			if dropped > 0 {
				return body, fmt.Errorf("kimi executor: failed to drop empty assistant messages: %w", errSet)
			}
			return body, fmt.Errorf("kimi executor: %s: %w", patches[0].errorContext, errSet)
		}
		out = updated
	}

	if patched > 0 || patchedReasoning > 0 {
		log.WithFields(log.Fields{
			"patched_tool_messages":      patched,
			"patched_reasoning_messages": patchedReasoning,
		}).Debug("kimi executor: normalized tool message fields")
	}
	if ambiguous > 0 {
		log.WithFields(log.Fields{
			"ambiguous_tool_messages": ambiguous,
			"pending_tool_calls":      len(pending),
		}).Warn("kimi executor: tool messages missing tool_call_id with ambiguous candidates")
	}
	return out, nil
}

func shouldDropKimiAssistantMessage(msg gjson.Result) bool {
	if strings.TrimSpace(msg.Get("role").String()) != "assistant" {
		return false
	}
	if hasKimiToolCalls(msg) || hasKimiLegacyFunctionCall(msg) || hasKimiAssistantReasoning(msg) {
		return false
	}
	return isKimiAssistantContentEmpty(msg.Get("content"))
}

func hasKimiToolCalls(msg gjson.Result) bool {
	toolCalls := msg.Get("tool_calls")
	return toolCalls.Exists() && toolCalls.IsArray() && len(toolCalls.Array()) > 0
}

func hasKimiLegacyFunctionCall(msg gjson.Result) bool {
	functionCall := msg.Get("function_call")
	if !functionCall.Exists() || functionCall.Type == gjson.Null {
		return false
	}
	if functionCall.IsObject() && strings.TrimSpace(functionCall.Raw) == "{}" {
		return false
	}
	return strings.TrimSpace(functionCall.Raw) != ""
}

func hasKimiAssistantReasoning(msg gjson.Result) bool {
	reasoning := msg.Get("reasoning_content")
	return reasoning.Exists() && strings.TrimSpace(reasoning.String()) != ""
}

func isKimiAssistantContentEmpty(content gjson.Result) bool {
	if !content.Exists() || content.Type == gjson.Null {
		return true
	}
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()) == ""
	}
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		if !isKimiAssistantContentPartEmpty(part) {
			return false
		}
	}
	return true
}

func isKimiAssistantContentPartEmpty(part gjson.Result) bool {
	if !part.Exists() || part.Type == gjson.Null {
		return true
	}
	if part.Type == gjson.String {
		return strings.TrimSpace(part.String()) == ""
	}
	if !part.IsObject() {
		return false
	}
	if text := part.Get("text"); text.Exists() {
		return strings.TrimSpace(text.String()) == ""
	}
	if strings.TrimSpace(part.Get("type").String()) == "text" {
		return true
	}
	return strings.TrimSpace(part.Raw) == "{}"
}

func isUsableKimiReasoning(reasoning string) bool {
	trimmed := strings.TrimSpace(reasoning)
	return trimmed != "" && trimmed != kimiReasoningUnavailable
}

func fallbackAssistantReasoning(msg gjson.Result, hasLatest bool, latest string) string {
	if hasLatest && isUsableKimiReasoning(latest) {
		return latest
	}

	content := msg.Get("content")
	if content.Type == gjson.String {
		if text := strings.TrimSpace(content.String()); text != "" {
			return text
		}
	}
	if content.IsArray() {
		parts := make([]string, 0, len(content.Array()))
		for _, item := range content.Array() {
			text := strings.TrimSpace(item.Get("text").String())
			if text == "" {
				continue
			}
			parts = append(parts, text)
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}

	return kimiReasoningUnavailable
}

// Refresh refreshes the Kimi token using the refresh token.
func (e *KimiExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("kimi executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("kimi executor: auth is nil")
	}
	// Expect refresh_token in metadata for OAuth-based accounts
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && strings.TrimSpace(v) != "" {
			refreshToken = v
		}
	}
	if strings.TrimSpace(refreshToken) == "" {
		// Nothing to refresh
		return auth, nil
	}

	client := kimiauth.NewDeviceFlowClientWithDeviceIDAndProxyURL(e.cfg, resolveKimiDeviceID(auth), auth.ProxyURL)
	td, err := client.RefreshToken(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	if td.ExpiresAt > 0 {
		exp := time.Unix(td.ExpiresAt, 0).UTC().Format(time.RFC3339)
		auth.Metadata["expired"] = exp
	}
	auth.Metadata["type"] = "kimi"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

// applyKimiHeaders sets required headers for Kimi API requests.
// Headers identify CLIProxyAPI with the current build version.
func applyKimiHeaders(r *http.Request, token string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	// Identify requests with the current CLIProxyAPI version.
	r.Header.Set("User-Agent", "CLIProxyAPI/"+buildinfo.Version)
	r.Header.Set("X-Msh-Platform", "CLIProxyAPI")
	r.Header.Set("X-Msh-Version", buildinfo.Version)
	r.Header.Set("X-Msh-Device-Name", getKimiHostname())
	r.Header.Set("X-Msh-Device-Model", getKimiDeviceModel())
	r.Header.Set("X-Msh-Device-Id", getKimiDeviceID())
	if stream {
		r.Header.Set("Accept", "text/event-stream")
		return
	}
	r.Header.Set("Accept", "application/json")
}

func resolveKimiDeviceIDFromAuth(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}

	deviceIDRaw, ok := auth.Metadata["device_id"]
	if !ok {
		return ""
	}

	deviceID, ok := deviceIDRaw.(string)
	if !ok {
		return ""
	}

	return strings.TrimSpace(deviceID)
}

func resolveKimiDeviceIDFromStorage(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}

	storage, ok := auth.Storage.(*kimiauth.KimiTokenStorage)
	if !ok || storage == nil {
		return ""
	}

	return strings.TrimSpace(storage.DeviceID)
}

func resolveKimiDeviceID(auth *cliproxyauth.Auth) string {
	deviceID := resolveKimiDeviceIDFromAuth(auth)
	if deviceID != "" {
		return deviceID
	}
	return resolveKimiDeviceIDFromStorage(auth)
}

func applyKimiHeadersWithAuth(r *http.Request, token string, stream bool, auth *cliproxyauth.Auth) {
	applyKimiHeaders(r, token, stream)

	if deviceID := resolveKimiDeviceID(auth); deviceID != "" {
		r.Header.Set("X-Msh-Device-Id", deviceID)
	}
}

// getKimiHostname returns the machine hostname.
func getKimiHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// getKimiDeviceModel returns a device model string matching kimi-cli format.
func getKimiDeviceModel() string {
	return fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH)
}

// getKimiDeviceID returns a stable device ID, matching kimi-cli storage location.
func getKimiDeviceID() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "cli-proxy-api-device"
	}
	// Check kimi-cli's device_id location first (platform-specific)
	var kimiShareDir string
	switch runtime.GOOS {
	case "darwin":
		kimiShareDir = filepath.Join(homeDir, "Library", "Application Support", "kimi")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(homeDir, "AppData", "Roaming")
		}
		kimiShareDir = filepath.Join(appData, "kimi")
	default: // linux and other unix-like
		kimiShareDir = filepath.Join(homeDir, ".local", "share", "kimi")
	}
	deviceIDPath := filepath.Join(kimiShareDir, "device_id")
	if data, err := os.ReadFile(deviceIDPath); err == nil {
		return strings.TrimSpace(string(data))
	}
	return "cli-proxy-api-device"
}

// kimiCreds extracts the access token from auth.
func kimiCreds(a *cliproxyauth.Auth) (token string) {
	if a == nil {
		return ""
	}
	// Check metadata first (OAuth flow stores tokens here)
	if a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	// Fallback to attributes (API key style)
	if a.Attributes != nil {
		if v := a.Attributes["access_token"]; v != "" {
			return v
		}
		if v := a.Attributes["api_key"]; v != "" {
			return v
		}
	}
	return ""
}

// stripKimiPrefix removes the "kimi-" prefix from model names for the upstream API.
func stripKimiPrefix(model string) string {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(strings.ToLower(model), "kimi-") {
		return model[5:]
	}
	return model
}

// normalizeKimiUpstreamModel returns the canonical upstream model ID for Kimi.
// It strips the CLIProxyAPI "kimi-" prefix and any Claude Code "[1m]" context
// suffix while preserving a trailing thinking suffix (e.g. "(1024)"), so that
// the upstream API receives IDs such as "k3(1024)" instead of "kimi-k3[1m](1024)".
// K2.7 Code aliases are remapped to the official Kimi Code model IDs before
// generic prefix stripping, so already-canonical IDs stay idempotent.
func normalizeKimiUpstreamModel(model string) string {
	model = strings.TrimSpace(model)
	parsed := thinking.ParseSuffix(model)
	base := strings.ToLower(strings.TrimSpace(parsed.ModelName))
	if strings.HasSuffix(base, "[1m]") {
		base = base[:len(base)-len("[1m]")]
	}
	var normalized string
	switch base {
	case "kimi-k2.7-code", "k2.7-code", "kimi-for-coding", "for-coding":
		normalized = "kimi-for-coding"
	case "kimi-k2.7-code-highspeed", "k2.7-code-highspeed", "kimi-for-coding-highspeed", "for-coding-highspeed":
		normalized = "kimi-for-coding-highspeed"
	default:
		normalized = stripKimiPrefix(base)
	}
	if parsed.HasSuffix {
		return normalized + "(" + parsed.RawSuffix + ")"
	}
	return normalized
}
