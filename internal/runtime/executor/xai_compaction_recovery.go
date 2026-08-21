package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"
)

const (
	xaiCompactionSummaryMaxOutputTokens = 8192
	xaiCompactionSummaryInstructions    = "Create a compact, self-contained text summary of the conversation for another model to continue from. Preserve user intent, important facts, decisions, constraints, unresolved work, and relevant tool results. Describe material information visible in images, files, or audio. Do not call tools. Return only the summary text."
	xaiCompactionInlineImagePlaceholder = "[Inline image payload omitted; use surrounding text and prior visual descriptions.]"
	xaiCompactionInlineFilePlaceholder  = "[Inline file payload omitted; use surrounding text and prior file descriptions.]"
	xaiCompactionInlineAudioPlaceholder = "[Inline audio payload omitted; use surrounding text and prior audio descriptions.]"
	xaiCompactContextErrorPrefix        = "compaction failed: This model's maximum prompt length is "
	xaiCompactContextErrorSeparator     = " but the request contains "
	xaiCompactContextErrorSuffix        = " tokens."
)

type xaiInlineMediaStats struct {
	count int
	chars int64
}

func xaiPreparedCompactTokenCount(body []byte) (int64, error) {
	enc, err := tokenizer.Get(tokenizer.O200kBase)
	if err != nil {
		return 0, fmt.Errorf("xai compact: tokenizer init failed: %w", err)
	}
	count, err := countXAIInputTokens(enc, body)
	if err != nil {
		return 0, fmt.Errorf("xai compact: token counting failed: %w", err)
	}
	return count, nil
}

func xaiModelContextLimit(model string) int64 {
	info := registry.LookupModelInfo(strings.TrimSpace(model), "xai")
	if info == nil {
		return 0
	}
	if info.MaxContextLength > 0 {
		return int64(info.MaxContextLength)
	}
	if info.ContextLength > 0 {
		return int64(info.ContextLength)
	}
	return 0
}

func xaiInlineMediaInPreparedBody(body []byte) xaiInlineMediaStats {
	var stats xaiInlineMediaStats
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return stats
	}
	for _, item := range input.Array() {
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "input_image":
				imageURL := part.Get("image_url")
				if imageURL.Type == gjson.String && strings.HasPrefix(strings.ToLower(strings.TrimSpace(imageURL.String())), "data:") {
					stats.count++
					stats.chars += int64(len(imageURL.String()))
				}
				stats.addInlineString(part.Get("file_data"))
			case "input_file":
				stats.addInlineString(part.Get("file_data"))
			case "input_audio":
				data := part.Get("data")
				if !data.Exists() {
					data = part.Get("input_audio.data")
				}
				stats.addInlineString(data)
			}
		}
	}
	return stats
}

func (s *xaiInlineMediaStats) addInlineString(value gjson.Result) {
	if s == nil || value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
		return
	}
	s.count++
	s.chars += int64(len(value.String()))
}

func xaiIsExactCompactContextLengthError(status int, body []byte) bool {
	if status != http.StatusBadRequest || !gjson.ValidBytes(body) {
		return false
	}
	root := gjson.ParseBytes(body)
	if root.Get("code").Type != gjson.String || root.Get("code").String() != "invalid-argument" || root.Get("error").Type != gjson.String {
		return false
	}
	message := root.Get("error").String()
	if !strings.HasPrefix(message, xaiCompactContextErrorPrefix) || !strings.HasSuffix(message, xaiCompactContextErrorSuffix) {
		return false
	}
	numbers := strings.TrimSuffix(strings.TrimPrefix(message, xaiCompactContextErrorPrefix), xaiCompactContextErrorSuffix)
	parts := strings.Split(numbers, xaiCompactContextErrorSeparator)
	if len(parts) != 2 {
		return false
	}
	limit, errLimit := xaiParsePositiveDecimal(parts[0])
	requestTokens, errRequest := xaiParsePositiveDecimal(parts[1])
	return errLimit == nil && errRequest == nil && limit > 0 && requestTokens > limit
}

func xaiParsePositiveDecimal(value string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("empty decimal")
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("invalid decimal")
		}
	}
	return strconv.ParseInt(value, 10, 64)
}

func xaiLogCompactionFallback(ctx context.Context, trigger string, estimatedTokens, contextLimit int64, media xaiInlineMediaStats) {
	helps.LogWithRequestID(ctx).WithFields(log.Fields{
		"compaction_provider": "xai",
		"compaction_phase":    "media_recovery",
		"fallback_trigger":    trigger,
		"estimated_tokens":    estimatedTokens,
		"context_limit":       contextLimit,
		"inline_media_count":  media.count,
		"inline_media_chars":  media.chars,
	}).Info("xai compaction media fallback")
}

func xaiBuildCompactionSummaryBody(fullCompactBody []byte, fallbackSessionID string) []byte {
	body := bytes.Clone(fullCompactBody)
	body, _ = sjson.SetBytes(body, "instructions", xaiCompactionSummaryInstructions)
	body, _ = sjson.SetBytes(body, "stream", true)
	body, _ = sjson.SetBytes(body, "max_output_tokens", xaiCompactionSummaryMaxOutputTokens)
	body, _ = sjson.SetBytes(body, "prompt_cache_key", fallbackSessionID)
	for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls", "previous_response_id"} {
		body, _ = sjson.DeleteBytes(body, field)
	}
	for _, itemType := range []string{"compaction_trigger", "additional_tools", xaiToolSearchOutputItemType} {
		body = xaiRemoveInputItemsByType(body, itemType)
	}
	return xaiSanitizeCompactionSummaryInlineMedia(body)
}

func xaiSanitizeCompactionSummaryInlineMedia(body []byte) []byte {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body
	}
	for inputIndex, item := range input.Array() {
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		for contentIndex, part := range content.Array() {
			placeholder := ""
			switch part.Get("type").String() {
			case "input_image":
				imageURL := strings.TrimSpace(part.Get("image_url").String())
				if strings.HasPrefix(strings.ToLower(imageURL), "data:") || xaiHasInlineMediaData(part.Get("file_data")) {
					placeholder = xaiCompactionInlineImagePlaceholder
				}
			case "input_file":
				if xaiHasInlineMediaData(part.Get("file_data")) {
					placeholder = xaiCompactionInlineFilePlaceholder
				}
			case "input_audio":
				if xaiHasInlineMediaData(part.Get("data")) || xaiHasInlineMediaData(part.Get("input_audio.data")) {
					placeholder = xaiCompactionInlineAudioPlaceholder
				}
			}
			if placeholder == "" {
				continue
			}
			path := fmt.Sprintf("input.%d.content.%d", inputIndex, contentIndex)
			body, _ = sjson.SetBytes(body, path, map[string]string{"type": "input_text", "text": placeholder})
		}
	}
	return body
}

func xaiHasInlineMediaData(value gjson.Result) bool {
	return value.Type == gjson.String && strings.TrimSpace(value.String()) != ""
}

func xaiBuildTextOnlyCompactBody(model, summary string) ([]byte, error) {
	body := struct {
		Model string `json:"model"`
		Input []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}{Model: model}
	body.Input = append(body.Input, struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{
		Type: "message",
		Role: "user",
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "input_text", Text: summary}},
	})
	return json.Marshal(body)
}

func (e *XAIExecutor) executeXAICompactionSummary(ctx context.Context, auth *cliproxyauth.Auth, prepared *xaiPreparedRequest, body []byte, fallbackSessionID string) (summary string, err error) {
	if prepared == nil {
		return "", statusErr{code: http.StatusInternalServerError, msg: "xai compact fallback summary request is unavailable"}
	}
	token, _ := xaiCreds(auth)
	requestURL := strings.TrimSuffix(xaiChatBaseURL(auth), "/") + "/responses"
	reporter := helps.NewExecutorUsageReporter(ctx, e, prepared.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(body, e.Identifier())

	helpFields := helps.CompactionDiagnosticFields(body)
	helpFields["compaction_provider"] = "xai"
	helpFields["compaction_phase"] = "fallback_summary_request"
	helpFields["upstream_url"] = requestURL
	helps.LogWithRequestID(ctx).WithFields(helpFields).Info("compaction capture")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	applyXAIChatHeaders(httpReq, auth, token, true, fallbackSessionID)
	e.recordXAIRequest(ctx, auth, requestURL, httpReq.Header.Clone(), body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return "", err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("xai executor: close compaction summary response body error: %v", errClose)
		}
	}()
	helpHeaders := httpResp.Header.Clone()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, helpHeaders)
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return "", err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return "", xaiStatusErr(httpResp.StatusCode, data)
	}

	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	pendingEventName := ""
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, xaiEventTag) {
			pendingEventName = strings.TrimSpace(string(line[len(xaiEventTag):]))
			continue
		}
		if !bytes.HasPrefix(line, xaiDataTag) {
			continue
		}
		eventData := xaiNormalizeReasoningSummaryData(bytes.TrimSpace(line[len(xaiDataTag):]))
		eventName := pendingEventName
		pendingEventName = ""
		if streamErr, ok := openAICompatStreamDataError(eventData, eventName); ok {
			normalized := xaiStatusErr(streamErr.code, []byte(streamErr.msg))
			return "", normalized
		}
		switch gjson.GetBytes(eventData, "type").String() {
		case "response.output_item.done":
			xaiCollectOutputItemDone(eventData, outputItemsByIndex, &outputItemsFallback)
		case "response.completed":
			completedData := xaiPatchCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
			completedData = xaiNormalizeReasoningSummaryData(completedData)
			if detail, ok := helps.ParseCodexUsage(completedData); ok {
				reporter.Publish(ctx, detail)
			}
			reporter.EnsurePublished(ctx)
			summary = xaiAssistantTextFromCompleted(completedData)
			if summary == "" {
				return "", statusErr{code: http.StatusBadGateway, msg: "xai compact fallback summary response has no assistant text"}
			}
			return summary, nil
		case "response.incomplete":
			incompleteData := xaiPatchCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
			incompleteData = xaiNormalizeReasoningSummaryData(incompleteData)
			if detail, ok := helps.ParseCodexUsage(incompleteData); ok {
				reporter.Publish(ctx, detail)
			}
			reporter.EnsurePublished(ctx)
			reason := strings.TrimSpace(gjson.GetBytes(incompleteData, "response.incomplete_details.reason").String())
			if reason != "max_output_tokens" {
				return "", statusErr{code: http.StatusBadGateway, msg: "xai compact fallback summary response was incomplete for an unsupported reason"}
			}
			summary = xaiAssistantTextFromCompleted(incompleteData)
			if summary == "" {
				return "", statusErr{code: http.StatusBadGateway, msg: "xai compact fallback summary response has no assistant text"}
			}
			return summary, nil
		}
	}
	return "", statusErr{code: http.StatusRequestTimeout, msg: "xai compact fallback summary stream disconnected before response.completed"}
}

func xaiAssistantTextFromCompleted(completedData []byte) string {
	output := gjson.GetBytes(completedData, "response.output")
	if !output.IsArray() {
		return ""
	}
	parts := make([]string, 0, 4)
	for _, item := range output.Array() {
		if item.Get("type").String() != "message" || !strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "assistant") {
			continue
		}
		content := item.Get("content")
		if content.Type == gjson.String {
			if text := strings.TrimSpace(content.String()); text != "" {
				parts = append(parts, text)
			}
			continue
		}
		for _, part := range content.Array() {
			if part.Get("type").String() != "output_text" && part.Get("type").String() != "text" {
				continue
			}
			if text := strings.TrimSpace(part.Get("text").String()); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func validateXAINativeCompactionResponse(data []byte) (string, []byte, error) {
	if len(data) == 0 || !json.Valid(data) {
		return "", nil, statusErr{code: http.StatusBadGateway, msg: "xai compaction returned invalid JSON"}
	}
	responseIDResult := gjson.GetBytes(data, "id")
	output := gjson.GetBytes(data, "output")
	if responseIDResult.Type != gjson.String || strings.TrimSpace(responseIDResult.String()) == "" || !output.IsArray() {
		return "", nil, statusErr{code: http.StatusBadGateway, msg: "xai compaction response is missing compacted state"}
	}
	items := output.Array()
	if len(items) != 1 {
		return "", nil, statusErr{code: http.StatusBadGateway, msg: "xai compaction response must contain exactly one compacted state item"}
	}
	item := items[0]
	encryptedContent := item.Get("encrypted_content")
	if item.Type != gjson.JSON || item.Get("type").Type != gjson.String || item.Get("type").String() != "compaction" ||
		encryptedContent.Type != gjson.String || strings.TrimSpace(encryptedContent.String()) == "" {
		return "", nil, statusErr{code: http.StatusBadGateway, msg: "xai compaction response is missing compacted state"}
	}
	if _, err := signature.InspectGrokEncryptedContent(encryptedContent.String()); err != nil {
		return "", nil, statusErr{code: http.StatusBadGateway, msg: "xai compaction response contains invalid compacted state"}
	}
	responseID := xaiCompactionResponseID(data)
	return responseID, xaiCompactionOutputItem(data, responseID), nil
}
