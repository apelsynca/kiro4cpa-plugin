package kiro

import (
	"encoding/json"
	"strings"
)

type sseEvent struct {
	name string
	data map[string]any
}

// Upstream tools may interleave, but CPA's Responses translator requires serial
// content blocks. Stream the first call immediately and queue later calls until
// it stops, retaining their arguments by ID.
type sseStream struct {
	responseState
	conv        convertedRequest
	parser      thinkingParser
	next        int
	openIdx     int
	openKind    string
	signature   string
	tools       toolAccumulator
	toolBlocks  map[string]*toolBlock
	toolCursor  int
	heldContent []Event
	output      tokenEstimate
}

type toolBlock struct {
	index int
	sent  int
}

func newSSEStream(conv convertedRequest) *sseStream {
	return &sseStream{conv: conv, openIdx: -1, toolBlocks: make(map[string]*toolBlock)}
}

func (s *sseStream) start() []sseEvent {
	return []sseEvent{{"message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + strings.ReplaceAll(newUUID(), "-", ""), "type": "message", "role": "assistant",
		"content": []any{}, "model": s.conv.ModelID, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": s.conv.InputTokens, "output_tokens": 1},
	}}}}
}

func (s *sseStream) handle(ev Event) []sseEvent {
	s.observe(ev)
	if s.streamErr != nil {
		return nil
	}
	if s.stopReason == "refusal" && len(s.tools.order) > 0 {
		s.streamErr = &StreamError{Kind: "error", Code: "ContentFiltered", Message: "upstream refused a response containing tool calls"}
		return nil
	}
	if s.toolCursor < len(s.tools.order) && (ev.Assistant != nil || ev.Reasoning != nil) {
		s.heldContent = append(s.heldContent, ev)
		return nil
	}
	switch {
	case ev.Assistant != nil && ev.Assistant.Content != "":
		s.output.add(ev.Assistant.Content)
		if !s.conv.Thinking {
			return s.text(ev.Assistant.Content)
		}
		return s.apply(s.parser.feed(ev.Assistant.Content))
	case ev.Reasoning != nil:
		if ev.Reasoning.Signature != "" {
			s.signature = ev.Reasoning.Signature
		}
		if t := ev.Reasoning.Text; t != "" {
			s.output.add(t)
			if s.conv.Thinking {
				return s.thinking(t)
			}
		}
	case ev.ToolUse != nil:
		return s.toolUse(*ev.ToolUse)
	}
	return nil
}

func (s *sseStream) finish() []sseEvent {
	if s.streamErr != nil {
		return nil
	}
	if s.stopReason == "refusal" && len(s.tools.order) > 0 {
		s.streamErr = &StreamError{Kind: "error", Code: "ContentFiltered", Message: "upstream refused a response containing tool calls"}
		return nil
	}
	if s.streamErr = s.tools.finish(); s.streamErr != nil {
		return nil
	}
	for _, id := range s.tools.order {
		// Complete empty no-argument tools which omitted their stop event.
		s.tools.tools[id].stopped = true
	}
	out := s.drainTools()
	out = append(out, s.apply(s.parser.flush())...)
	out = append(out, s.closeBlock()...)
	return append(out,
		sseEvent{"message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": s.completionReason(len(s.tools.order) > 0), "stop_sequence": nil},
			"usage": usageFields(s.usage, s.conv.InputTokens, s.output.tokens())}},
		sseEvent{"message_stop", map[string]any{"type": "message_stop"}},
	)
}

func (s *sseStream) apply(segs []segment) []sseEvent {
	var out []sseEvent
	for _, seg := range segs {
		switch seg.kind {
		case segText:
			out = append(out, s.text(seg.text)...)
		case segThinking:
			out = append(out, s.thinking(seg.text)...)
		case segThinkingEnd:
			if s.openKind == "thinking" {
				out = append(out, s.closeBlock()...)
			}
		}
	}
	return out
}

func (s *sseStream) text(t string) []sseEvent {
	var out []sseEvent
	if s.openKind != "text" {
		out = s.openBlock("text", map[string]any{"type": "text", "text": ""})
	}
	return append(out, s.delta(map[string]any{"type": "text_delta", "text": t}))
}

func (s *sseStream) thinking(t string) []sseEvent {
	var out []sseEvent
	if s.openKind != "thinking" {
		out = s.openBlock("thinking", map[string]any{"type": "thinking", "thinking": ""})
	}
	return append(out, s.delta(map[string]any{"type": "thinking_delta", "thinking": t}))
}

func (s *sseStream) toolUse(tu ToolUseEvent) []sseEvent {
	if s.stopReason == "refusal" {
		s.streamErr = &StreamError{Kind: "error", Code: "ContentFiltered", Message: "tool call arrived after upstream refusal"}
		return nil
	}
	if err := s.tools.add(tu); err != nil {
		s.streamErr = err
		return nil
	}
	s.output.add(tu.Input)
	out := s.apply(s.parser.flush())
	return append(out, s.drainTools()...)
}

func (s *sseStream) drainTools() []sseEvent {
	var out []sseEvent
	for s.toolCursor < len(s.tools.order) {
		id := s.tools.order[s.toolCursor]
		t := s.tools.tools[id]
		out = append(out, s.emitTool(id, t)...)
		if !t.stopped || t.name == "" {
			return out
		}
		s.toolCursor++
	}
	held := s.heldContent
	s.heldContent = nil
	for _, ev := range held {
		out = append(out, s.handle(ev)...)
	}
	return out
}

func (s *sseStream) emitTool(id string, t *collectedTool) []sseEvent {
	// Retain fragments until the tool name arrives.
	if t.name == "" {
		return nil
	}
	block := s.toolBlocks[id]
	var out []sseEvent
	if block == nil {
		out = s.closeBlock()
		name := t.name
		if original, ok := s.conv.ToolNames[name]; ok {
			name = original
		}
		block = &toolBlock{index: s.next}
		s.next++
		s.toolBlocks[id] = block
		out = append(out, sseEvent{"content_block_start", map[string]any{"type": "content_block_start", "index": block.index,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}}})
	}
	input := t.input.String()
	if t.stopped && strings.TrimSpace(input) == "" {
		input += "{}"
	}
	if len(input) > block.sent {
		out = append(out, sseEvent{"content_block_delta", map[string]any{"type": "content_block_delta", "index": block.index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": input[block.sent:]}}})
		block.sent = len(input)
	}
	if t.stopped {
		out = append(out, sseEvent{"content_block_stop", map[string]any{"type": "content_block_stop", "index": block.index}})
	}
	return out
}

func (s *sseStream) openBlock(kind string, block map[string]any) []sseEvent {
	out := s.closeBlock()
	s.openIdx, s.openKind = s.next, kind
	s.next++
	return append(out, sseEvent{"content_block_start", map[string]any{"type": "content_block_start", "index": s.openIdx, "content_block": block}})
}

func (s *sseStream) delta(d map[string]any) sseEvent {
	return sseEvent{"content_block_delta", map[string]any{"type": "content_block_delta", "index": s.openIdx, "delta": d}}
}

func (s *sseStream) closeBlock() []sseEvent {
	if s.openIdx < 0 {
		return nil
	}
	var out []sseEvent
	if s.openKind == "thinking" && s.signature != "" {
		out = append(out, s.delta(map[string]any{"type": "signature_delta", "signature": s.signature}))
	}
	out = append(out, sseEvent{"content_block_stop", map[string]any{"type": "content_block_stop", "index": s.openIdx}})
	s.openIdx, s.openKind = -1, ""
	return out
}

// sseChunks emits one SSE line per chunk: CPA's Claude->OpenAI translators test
// each chunk for a "data:" prefix and never split lines themselves.
func sseChunks(events []sseEvent) [][]byte {
	out := make([][]byte, 0, len(events)*2)
	for _, e := range events {
		data, _ := json.Marshal(e.data)
		out = append(out, []byte("event: "+e.name+"\n"), append(append([]byte("data: "), data...), '\n', '\n'))
	}
	return out
}
