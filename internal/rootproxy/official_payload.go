package rootproxy

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	multiagentv2 "github.com/router-for-me/CLIProxyAPI/v7/internal/client/codex/optimize-multi-agent-v2"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/signature"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var errNonPortableCompaction = errors.New("cross-provider compaction state is not portable to the selected upstream")

const kimiCompactionPrefix = "kimi-compaction-v1:"

// officialFastServiceTier is the wire value behind the catalog's "Fast" speed
// tier, which every stock entry advertises as service_tiers[].id.
const officialFastServiceTier = "priority"

// normalizeRelayMultiAgentParentPayload keeps delegated message text portable
// when Root advertises Relay-hosted multi-agent workers. The official service
// reserves the collaboration schema, so Root moves it to a non-reserved alias
// before removing delivery-message encryption markers. Responses must restore
// the client-visible namespace.
func normalizeRelayMultiAgentParentPayload(payload []byte, enabled bool) ([]byte, bool) {
	if !enabled {
		return payload, false
	}
	return multiagentv2.PrepareCodexRelayDelegationRequest(payload)
}

func restoreRelayMultiAgentResponse(payload []byte, optimized bool) []byte {
	return multiagentv2.RestoreCodexMultiAgentV2Response(payload, optimized)
}

func payloadContainsCompaction(payload []byte) bool {
	return payloadContainsInputType(payload, "compaction")
}

func payloadContainsCompactionTrigger(payload []byte) bool {
	return payloadContainsInputType(payload, "compaction_trigger")
}

func payloadContainsInputType(payload []byte, expected string) bool {
	input := gjson.GetBytes(payload, "input")
	if !input.Exists() || !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) == expected {
			return true
		}
	}
	return false
}

// validateRelayPayloadState rejects compaction that cannot be attributed to
// the configured target provider. A compaction creation request also requires
// an attributed target even when it does not carry an earlier compaction item.
func validateRelayPayloadState(payload []byte, target relayProvider, createsCompaction bool) error {
	if target == "" && (createsCompaction || payloadContainsCompactionTrigger(payload) || payloadContainsCompaction(payload)) {
		return errors.New("Relay compaction requires a configured relay model provider")
	}

	input := gjson.GetBytes(payload, "input")
	if !input.Exists() || !input.IsArray() {
		return nil
	}
	for index, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "compaction" {
			continue
		}
		encrypted := item.Get("encrypted_content")
		if encrypted.Type != gjson.String || strings.TrimSpace(encrypted.String()) != encrypted.String() {
			return fmt.Errorf("%w at input[%d]", errNonPortableCompaction, index)
		}
		source := inspectRelayCompactionProvider(encrypted.String())
		if source == "" || source != target {
			return fmt.Errorf("%w at input[%d]", errNonPortableCompaction, index)
		}
	}
	return nil
}

func inspectRelayCompactionProvider(encryptedContent string) relayProvider {
	if strings.HasPrefix(encryptedContent, kimiCompactionPrefix) {
		encoded := strings.TrimPrefix(encryptedContent, kimiCompactionPrefix)
		decoded, errDecode := base64.StdEncoding.DecodeString(encoded)
		if errDecode == nil && strings.TrimSpace(string(decoded)) != "" {
			return relayProviderKimi
		}
		return ""
	}
	if _, errInspect := signature.InspectGrokEncryptedContent(encryptedContent); errInspect == nil {
		return relayProviderXAI
	}
	return ""
}

// applyOfficialFastServiceTier forces the ChatGPT "Fast" service tier on stock
// turns for the models an operator opted in. Desktop expresses the tier as a
// top-level service_tier field it only sends when the user picks Fast by hand,
// so Root writes the same value it would have sent. The payload is returned
// untouched for every other model, and when the tier is already selected, so an
// unchanged body keeps its original encoding on the way upstream.
//
// Only turn-creating requests may carry it: /responses/compact is background
// summarization that would spend the tier's higher usage for no latency the
// user can perceive, and a non-create WebSocket frame is not a request at all.
func applyOfficialFastServiceTier(payload []byte, model string, fastModels map[string]struct{}) ([]byte, error) {
	if len(fastModels) == 0 {
		return payload, nil
	}
	if _, fast := fastModels[model]; !fast {
		return payload, nil
	}
	if gjson.GetBytes(payload, "service_tier").String() == officialFastServiceTier {
		return payload, nil
	}
	updated, errSet := sjson.SetBytes(payload, "service_tier", officialFastServiceTier)
	if errSet != nil {
		return nil, fmt.Errorf("set official service tier for model %q: %w", model, errSet)
	}
	return updated, nil
}

// prepareOfficialPayload removes only provider-bound reasoning state that the
// official upstream cannot consume. Valid GPT opaque state and all ordinary
// request bytes remain unchanged. Foreign compaction cannot be discarded
// without losing conversation history, so it fails closed instead.
func prepareOfficialPayload(payload []byte) ([]byte, error) {
	input := gjson.GetBytes(payload, "input")
	if !input.Exists() || !input.IsArray() {
		return payload, nil
	}

	items := input.Array()
	stripOrphanIDs := !gjson.GetBytes(payload, "store").Bool()
	var rebuilt []byte
	itemsWritten := 0
	keep := func(raw string) {
		if rebuilt == nil {
			return
		}
		if itemsWritten > 0 {
			rebuilt = append(rebuilt, ',')
		}
		rebuilt = append(rebuilt, raw...)
		itemsWritten++
	}
	startRebuild := func(index int) {
		if rebuilt != nil {
			return
		}
		rebuilt = make([]byte, 0, len(input.Raw))
		rebuilt = append(rebuilt, '[')
		for preceding := range index {
			keep(items[preceding].Raw)
		}
	}

	for index, item := range items {
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType != "reasoning" && itemType != "compaction" {
			keep(item.Raw)
			continue
		}

		encrypted := item.Get("encrypted_content")
		validGPTState := encrypted.Type == gjson.String && strings.TrimSpace(encrypted.String()) == encrypted.String()
		if validGPTState {
			_, errInspect := signature.InspectGPTReasoningSignature(encrypted.String())
			validGPTState = errInspect == nil
		}
		if itemType == "compaction" {
			if !validGPTState {
				return nil, fmt.Errorf("%w at input[%d]", errNonPortableCompaction, index)
			}
			keep(item.Raw)
			continue
		}

		if validGPTState {
			keep(item.Raw)
			continue
		}

		nextItem := item.Raw
		changed := false
		if encrypted.Exists() {
			var errDelete error
			nextItem, errDelete = sjson.Delete(nextItem, "encrypted_content")
			if errDelete != nil {
				return nil, fmt.Errorf("remove foreign reasoning state at input[%d]: %w", index, errDelete)
			}
			changed = true
		}
		if stripOrphanIDs && item.Get("id").Exists() {
			var errDeleteID error
			nextItem, errDeleteID = sjson.Delete(nextItem, "id")
			if errDeleteID != nil {
				return nil, fmt.Errorf("remove foreign reasoning id at input[%d]: %w", index, errDeleteID)
			}
			changed = true
		}
		if changed {
			startRebuild(index)
		}
		keep(nextItem)
	}

	if rebuilt == nil {
		return payload, nil
	}
	rebuilt = append(rebuilt, ']')
	updated, errSet := sjson.SetRawBytes(payload, "input", rebuilt)
	if errSet != nil {
		return nil, fmt.Errorf("rebuild official request input: %w", errSet)
	}
	return updated, nil
}
