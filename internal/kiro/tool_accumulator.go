package kiro

import (
	"encoding/json"
	"strings"
)

type collectedTool struct {
	name    string
	input   strings.Builder
	stopped bool
}

type toolAccumulator struct {
	order []string
	tools map[string]*collectedTool
}

func (a *toolAccumulator) add(ev ToolUseEvent) *StreamError {
	if ev.ToolUseID == "" {
		return toolStreamError("missing tool call ID")
	}
	if a.tools == nil {
		a.tools = make(map[string]*collectedTool)
	}
	t := a.tools[ev.ToolUseID]
	if t == nil {
		t = &collectedTool{}
		a.tools[ev.ToolUseID] = t
		a.order = append(a.order, ev.ToolUseID)
	}
	if ev.Name != "" {
		if t.name != "" && t.name != ev.Name {
			return toolStreamError("tool name changed within a call")
		}
		t.name = ev.Name
	}
	if t.stopped {
		if ev.Input != "" {
			return toolStreamError("tool input arrived after stop")
		}
		return nil // tolerate a duplicate empty stop
	}
	t.input.WriteString(ev.Input)
	if ev.Stop {
		t.stopped = true
		return t.validate()
	}
	return nil
}

func (t *collectedTool) validate() *StreamError {
	if strings.TrimSpace(t.name) == "" {
		return toolStreamError("missing tool name")
	}
	input := strings.TrimSpace(t.input.String())
	// Kiro can omit both the input and stop for a no-argument tool.
	if input == "" {
		return nil
	}
	if !t.stopped {
		return toolStreamError("tool input truncated before stop")
	}
	if input[0] != '{' || !json.Valid([]byte(input)) {
		return toolStreamError("tool input is not a complete JSON object")
	}
	return nil
}

func (a *toolAccumulator) finish() *StreamError {
	for _, id := range a.order {
		if err := a.tools[id].validate(); err != nil {
			return err
		}
	}
	return nil
}

func (t *collectedTool) arguments() string {
	if strings.TrimSpace(t.input.String()) == "" {
		return "{}"
	}
	return t.input.String()
}
