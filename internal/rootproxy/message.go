package rootproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type clientMessageEnvelope struct {
	eventType                string
	hasEventType             bool
	model                    string
	hasModel                 bool
	previousResponseID       string
	hasPreviousResponseID    bool
	previousResponseIDIsNull bool
	hasConversation          bool
	conversationIsNull       bool
}

func inspectClientMessage(payload []byte) (clientMessageEnvelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, errOpening := decoder.Token()
	if errOpening != nil {
		return clientMessageEnvelope{}, fmt.Errorf("decode client message: %w", errOpening)
	}
	openingObject, ok := opening.(json.Delim)
	if !ok || openingObject != '{' {
		return clientMessageEnvelope{}, errors.New("client message must be a JSON object")
	}

	var envelope clientMessageEnvelope
	for decoder.More() {
		keyToken, errKey := decoder.Token()
		if errKey != nil {
			return clientMessageEnvelope{}, fmt.Errorf("decode client message key: %w", errKey)
		}
		key, okKey := keyToken.(string)
		if !okKey {
			return clientMessageEnvelope{}, errors.New("client message contains a non-string key")
		}

		var raw json.RawMessage
		if errValue := decoder.Decode(&raw); errValue != nil {
			return clientMessageEnvelope{}, fmt.Errorf("decode client message field %q: %w", key, errValue)
		}
		switch key {
		case "type":
			if envelope.hasEventType {
				return clientMessageEnvelope{}, errors.New("client message contains duplicate type fields")
			}
			if errType := json.Unmarshal(raw, &envelope.eventType); errType != nil {
				return clientMessageEnvelope{}, errors.New("client message type must be a string")
			}
			envelope.hasEventType = true
		case "model":
			if envelope.hasModel {
				return clientMessageEnvelope{}, errors.New("client message contains duplicate model fields")
			}
			if errModel := json.Unmarshal(raw, &envelope.model); errModel != nil {
				return clientMessageEnvelope{}, errors.New("client message model must be a string")
			}
			envelope.hasModel = true
		case "previous_response_id":
			if envelope.hasPreviousResponseID {
				return clientMessageEnvelope{}, errors.New("client message contains duplicate previous_response_id fields")
			}
			envelope.hasPreviousResponseID = true
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				envelope.previousResponseIDIsNull = true
				continue
			}
			if errPrevious := json.Unmarshal(raw, &envelope.previousResponseID); errPrevious != nil {
				return clientMessageEnvelope{}, errors.New("client message previous_response_id must be a string or null")
			}
		case "conversation":
			if envelope.hasConversation {
				return clientMessageEnvelope{}, errors.New("client message contains duplicate conversation fields")
			}
			envelope.hasConversation = true
			trimmed := bytes.TrimSpace(raw)
			envelope.conversationIsNull = bytes.Equal(trimmed, []byte("null"))
			if envelope.conversationIsNull {
				continue
			}
			if len(trimmed) == 0 || (trimmed[0] != '"' && trimmed[0] != '{') {
				return clientMessageEnvelope{}, errors.New("client message conversation must be a string, object, or null")
			}
		}
	}

	closing, errClosing := decoder.Token()
	if errClosing != nil {
		return clientMessageEnvelope{}, fmt.Errorf("decode client message closing token: %w", errClosing)
	}
	closingObject, ok := closing.(json.Delim)
	if !ok || closingObject != '}' {
		return clientMessageEnvelope{}, errors.New("client message is not a complete JSON object")
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); !errors.Is(errTrailing, io.EOF) {
		if errTrailing == nil {
			return clientMessageEnvelope{}, errors.New("client message contains multiple JSON values")
		}
		return clientMessageEnvelope{}, fmt.Errorf("decode trailing client message: %w", errTrailing)
	}
	return envelope, nil
}

func (e clientMessageEnvelope) referencesUpstreamState() bool {
	previousResponse := e.hasPreviousResponseID && !e.previousResponseIDIsNull
	conversation := e.hasConversation && !e.conversationIsNull
	return previousResponse || conversation
}

func inspectHTTPModel(payload []byte) (string, error) {
	envelope, errInspect := inspectClientMessage(payload)
	if errInspect != nil {
		return "", errInspect
	}
	if !envelope.hasModel || envelope.model == "" {
		return "", errors.New("request must contain a non-empty model")
	}
	return envelope.model, nil
}

func upstreamEventIsTerminal(payload []byte) bool {
	_, terminal := upstreamTerminalOutcome(payload)
	return terminal
}

func upstreamTerminalOutcome(payload []byte) (string, bool) {
	envelope, errInspect := inspectClientMessage(payload)
	if errInspect != nil || !envelope.hasEventType {
		return "", false
	}
	switch envelope.eventType {
	case "response.completed", "response.done":
		return "completed", true
	case "response.failed":
		return "failed", true
	case "response.incomplete":
		return "incomplete", true
	case "response.cancelled", "response.canceled":
		return "canceled", true
	default:
		return "", false
	}
}

func upstreamEventIsError(payload []byte) bool {
	envelope, errInspect := inspectClientMessage(payload)
	return errInspect == nil && envelope.hasEventType && envelope.eventType == "error"
}

func inspectFirstClientMessage(payload []byte) (string, error) {
	envelope, errInspect := inspectClientMessage(payload)
	if errInspect != nil {
		return "", errInspect
	}
	if !envelope.hasEventType || envelope.eventType != "response.create" {
		return "", errors.New("first client message must have type response.create")
	}
	if !envelope.hasModel || envelope.model == "" {
		return "", errors.New("first response.create must contain a non-empty model")
	}
	return envelope.model, nil
}
