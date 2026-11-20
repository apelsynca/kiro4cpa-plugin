package kiro

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/models"
)

const defaultThinkingBudget = 20000

var errNoMessages = errors.New("request has no user message")

type convertedRequest struct {
	Payload     kiroPayload
	ModelID     string
	Thinking    bool
	ToolNames   map[string]string // shortened Kiro name -> client name
	InputTokens int
}

func convertClaudeRequest(raw []byte) (convertedRequest, error) {
	var req claudeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return convertedRequest{}, fmt.Errorf("decode claude request: %w", err)
	}
	modelID, suffixThinking := kiroModelID(req.Model)
	messages := trimPrefill(req.Messages)
	if len(messages) == 0 {
		return convertedRequest{}, errNoMessages
	}
	prefix := thinkingPrefix(req, suffixThinking)

	toolNames := map[string]string{}
	tools := convertTools(req.Tools, toolNames)
	currentStart := len(messages) - 1
	for currentStart > 0 && messages[currentStart-1].Role == "user" {
		currentStart--
	}
	history := buildHistory(req, messages[:currentStart], prefix, modelID, toolNames)
	current := mergeUsers(messages[currentStart:], modelID)
	var currentResults []kiroToolResult
	if current.UserInputMessageContext != nil {
		currentResults = current.UserInputMessageContext.ToolResults
	}
	results, orphans := pairToolResults(history, currentResults)
	history = removeToolUses(history, orphans)
	tools = appendPlaceholderTools(tools, history)

	ctx := &inputContext{
		EnvState:    &envState{OperatingSystem: "macos", CurrentWorkingDirectory: "/"},
		Tools:       tools,
		ToolResults: results,
	}
	state := conversationState{
		ConversationID:      conversationID(req),
		AgentContinuationID: newUUID(),
		AgentTaskType:       "vibe",
		ChatTriggerType:     "MANUAL",
		CurrentMessage: currentMessage{UserInputMessage: userInputMessage{
			Content: current.Content, ModelID: modelID, Origin: "AI_EDITOR",
			Images: current.Images, UserInputMessageContext: ctx,
		}},
		History: history,
	}
	return convertedRequest{
		Payload:     kiroPayload{ConversationState: state, AdditionalModelRequestFields: nativeReasoningFields(req, modelID, suffixThinking)},
		ModelID:     modelID,
		Thinking:    thinkingEnabled(req, suffixThinking),
		ToolNames:   toolNames,
		InputTokens: estimateRequestTokens(req),
	}, nil
}

// trimPrefill drops trailing assistant turns: Kiro does not support prefill.
func trimPrefill(messages []claudeMessage) []claudeMessage {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[:i+1]
		}
	}
	return nil
}

func thinkingPrefix(req claudeRequest, suffixThinking bool) string {
	modelID, _ := kiroModelID(req.Model)
	if nativeReasoningFields(req, modelID, suffixThinking) != nil {
		return ""
	}
	t := req.Thinking
	if t != nil && t.Type == "disabled" {
		return ""
	}
	if !thinkingEnabled(req, suffixThinking) {
		return ""
	}
	if (t != nil && t.Type == "adaptive") || supportsNativeReasoning(modelID) {
		return fmt.Sprintf("<thinking_mode>adaptive</thinking_mode><thinking_effort>%s</thinking_effort>", reasoningEffort(req, modelID))
	}
	budget := defaultThinkingBudget
	if t != nil && t.BudgetTokens > 0 {
		budget = min(t.BudgetTokens, models.MaxThinkingBudget)
	}
	return fmt.Sprintf("<thinking_mode>enabled</thinking_mode><max_thinking_length>%d</max_thinking_length>", budget)
}

func thinkingEnabled(req claudeRequest, suffixThinking bool) bool {
	if req.Thinking != nil && req.Thinking.Type == "disabled" {
		return false
	}
	return suffixThinking || req.Thinking != nil && (req.Thinking.Type == "enabled" || req.Thinking.Type == "adaptive")
}

func hasThinkingTags(s string) bool {
	return strings.Contains(s, "<thinking_mode>") || strings.Contains(s, "<max_thinking_length>")
}
