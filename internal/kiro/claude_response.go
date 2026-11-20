package kiro

import (
	"encoding/json"
	"strings"
)

// responseCollector folds a complete Kiro event stream into one Claude message.
type responseCollector struct {
	responseState
	text      strings.Builder
	reasoning strings.Builder
	signature string
	tools     toolAccumulator
}

func (c *responseCollector) add(ev Event) {
	c.observe(ev)
	if c.streamErr != nil {
		return
	}
	switch {
	case ev.Assistant != nil:
		c.text.WriteString(ev.Assistant.Content)
	case ev.Reasoning != nil:
		c.reasoning.WriteString(ev.Reasoning.Text)
		if ev.Reasoning.Signature != "" {
			c.signature = ev.Reasoning.Signature
		}
	case ev.ToolUse != nil:
		c.streamErr = c.tools.add(*ev.ToolUse)
	}
}

func (c *responseCollector) validate() *StreamError {
	if c.streamErr != nil {
		return c.streamErr
	}
	if c.stopReason == "refusal" && len(c.tools.order) > 0 {
		return &StreamError{Kind: "error", Code: "ContentFiltered", Message: "upstream refused a response containing tool calls"}
	}
	return c.tools.finish()
}

func (c *responseCollector) message(conv convertedRequest) []byte {
	text := c.text.String()
	thinking := c.reasoning.String()
	if conv.Thinking && thinking == "" {
		thinking, text = extractThinking(text)
	}
	if !conv.Thinking {
		thinking = ""
	}
	content := []map[string]any{}
	if thinking != "" {
		block := map[string]any{"type": "thinking", "thinking": thinking}
		if c.signature != "" {
			block["signature"] = c.signature
		}
		content = append(content, block)
	}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, id := range c.tools.order {
		t := c.tools.tools[id]
		input := json.RawMessage(t.arguments())
		name := t.name
		if original, ok := conv.ToolNames[name]; ok {
			name = original
		}
		content = append(content, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
	}
	stop := c.completionReason(len(c.tools.order) > 0)
	var output tokenEstimate
	output.add(c.text.String() + c.reasoning.String())
	for _, id := range c.tools.order {
		output.add(c.tools.tools[id].input.String())
	}
	out, _ := json.Marshal(map[string]any{
		"id": "msg_" + strings.ReplaceAll(newUUID(), "-", ""), "type": "message", "role": "assistant",
		"model": conv.ModelID, "content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": usageFields(c.usage, conv.InputTokens, output.tokens()),
	})
	return out
}

// usageFields prefers Kiro's real token counts; input_tokens excludes cached
// tokens, matching Anthropic's usage semantics.
func usageFields(u *TokenUsage, estimatedInput, estimatedOutput int) map[string]int64 {
	if u != nil {
		return map[string]int64{
			"input_tokens":                max(u.UncachedInputTokens, 0),
			"output_tokens":               max(u.OutputTokens, 0),
			"cache_read_input_tokens":     max(u.CacheReadInputTokens, 0),
			"cache_creation_input_tokens": max(u.CacheWriteInputTokens, 0),
		}
	}
	return map[string]int64{"input_tokens": int64(estimatedInput), "output_tokens": int64(estimatedOutput)}
}
