package rootproxy

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	createThreadToolExplicitGate    = "Create a separate task only when the user explicitly asks for a new task."
	createThreadToolAutonomousGate  = "Create a separate task when the user explicitly asks for a new task or when owner-authorized autonomous orchestration requires a durable worker."
	createThreadCoordExplicitThread = "Only use `create_thread` when the user explicitly asks to create a new thread."
	createThreadCoordExplicitTask   = "Only use `create_thread` when the user explicitly asks for a new task."
	createThreadCoordAutonomousGate = "Use `create_thread` when the user explicitly asks for a new task or when owner-authorized autonomous orchestration requires a durable worker."
)

// rewriteCreateThreadSchema removes Desktop's explicit-user-request gate from
// create_thread tool descriptions on turn-creating Root payloads. The matching
// Thread Coordination sentences in instructions and developer/system input are
// rewritten with the same autonomous-orchestration exception so the model does
// not keep a contradictory copy of the gate. chatgptWorkCloud, saved-project,
// and subagent rules are left unchanged.
func rewriteCreateThreadSchema(payload []byte) []byte {
	updated := payload
	changed := false

	for _, toolPath := range createThreadToolPaths(updated) {
		description := gjson.GetBytes(updated, toolPath+".description")
		if description.Type != gjson.String {
			continue
		}
		next := rewriteCreateThreadToolDescription(description.String())
		if next == description.String() {
			continue
		}
		var errSet error
		updated, errSet = sjson.SetBytes(updated, toolPath+".description", next)
		if errSet != nil {
			return payload
		}
		changed = true
	}

	if instructions := gjson.GetBytes(updated, "instructions"); instructions.Type == gjson.String {
		next := rewriteCreateThreadInstructionText(instructions.String())
		if next != instructions.String() {
			var errSet error
			updated, errSet = sjson.SetBytes(updated, "instructions", next)
			if errSet != nil {
				return payload
			}
			changed = true
		}
	}

	input := gjson.GetBytes(updated, "input")
	if input.IsArray() {
		for index, item := range input.Array() {
			role := strings.TrimSpace(item.Get("role").String())
			if role != "developer" && role != "system" {
				continue
			}
			rewritten, ok := rewriteCreateThreadInputContent(updated, fmt.Sprintf("input.%d.content", index), item.Get("content"))
			if !ok {
				return payload
			}
			if rewritten != nil {
				updated = rewritten
				changed = true
			}
		}
	}

	if !changed {
		return payload
	}
	return updated
}

func rewriteCreateThreadToolDescription(description string) string {
	return strings.ReplaceAll(description, createThreadToolExplicitGate, createThreadToolAutonomousGate)
}

func rewriteCreateThreadInstructionText(text string) string {
	text = strings.ReplaceAll(text, createThreadCoordExplicitThread, createThreadCoordAutonomousGate)
	return strings.ReplaceAll(text, createThreadCoordExplicitTask, createThreadCoordAutonomousGate)
}

func rewriteCreateThreadInputContent(payload []byte, path string, content gjson.Result) ([]byte, bool) {
	if content.Type == gjson.String {
		next := rewriteCreateThreadInstructionText(content.String())
		if next == content.String() {
			return nil, true
		}
		updated, errSet := sjson.SetBytes(payload, path, next)
		if errSet != nil {
			return nil, false
		}
		return updated, true
	}
	if !content.IsArray() {
		return nil, true
	}
	updated := payload
	changed := false
	for index, part := range content.Array() {
		partType := strings.TrimSpace(part.Get("type").String())
		if partType != "input_text" && partType != "text" {
			continue
		}
		text := part.Get("text")
		if text.Type != gjson.String {
			continue
		}
		next := rewriteCreateThreadInstructionText(text.String())
		if next == text.String() {
			continue
		}
		var errSet error
		updated, errSet = sjson.SetBytes(updated, fmt.Sprintf("%s.%d.text", path, index), next)
		if errSet != nil {
			return nil, false
		}
		changed = true
	}
	if !changed {
		return nil, true
	}
	return updated, true
}

func createThreadToolPaths(payload []byte) []string {
	paths := make([]string, 0, 1)
	collectCreateThreadToolPaths(gjson.GetBytes(payload, "tools"), "tools", &paths)

	input := gjson.GetBytes(payload, "input")
	if input.IsArray() {
		for index, item := range input.Array() {
			if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
				continue
			}
			collectCreateThreadToolPaths(item.Get("tools"), fmt.Sprintf("input.%d.tools", index), &paths)
		}
	}
	return paths
}

func collectCreateThreadToolPaths(tools gjson.Result, path string, paths *[]string) {
	if !tools.IsArray() {
		return
	}
	for index, tool := range tools.Array() {
		toolPath := fmt.Sprintf("%s.%d", path, index)
		toolType := strings.TrimSpace(tool.Get("type").String())
		name := strings.TrimSpace(tool.Get("name").String())
		if toolType == "function" && isCreateThreadToolName(name) {
			*paths = append(*paths, toolPath)
		}
		if toolType == "namespace" {
			collectCreateThreadToolPaths(tool.Get("tools"), toolPath+".tools", paths)
		}
	}
}

func isCreateThreadToolName(name string) bool {
	return name == "create_thread" || name == "mcp__codex_app__create_thread" || strings.HasSuffix(name, "__create_thread")
}
