package kiro

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AkkunYo/kiro4cpa-plugin/internal/hostapi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func TestCoreRegressionIncompleteToolNonstream(t *testing.T) {
	stubHTTP(t, func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		return &hostapi.HTTPResponse{StatusCode: 200, Body: eventFrame("toolUseEvent", `{"name":"Read","toolUseId":"t1","input":"{\"path\":"}`)}, nil
	})
	raw, _ := json.Marshal(execRequest(toolRequest))
	out, _ := HandleMethod(pluginabi.MethodExecutorExecute, raw)
	env := decodeEnvelope(t, out)
	if env.OK {
		var result struct {
			Payload []byte `json:"payload"`
		}
		_ = json.Unmarshal(env.Result, &result)
		t.Fatalf("incomplete tool input returned success: %s", result.Payload)
	}
}

func TestCoreRegressionIncompleteToolStream(t *testing.T) {
	s := stubStream(t, &streamStub{status: 200, reads: []hostapi.StreamChunk{{Payload: eventFrame("toolUseEvent", `{"name":"Read","toolUseId":"t1","input":"{\"path\":"}`), Done: true}}})
	if env := streamCall(t, toolRequest); !env.OK {
		t.Fatal("fixture did not open stream")
	}
	msg := waitClosed(t, s)
	if msg == "" || strings.Contains(strings.Join(s.emitted, ""), "message_stop") {
		t.Fatalf("incomplete tool input ended normally: close=%q message_stop=%v", msg, strings.Contains(strings.Join(s.emitted, ""), "message_stop"))
	}
}

func TestCoreRegressionSplit403Refresh(t *testing.T) {
	s := stubStream(t, &streamStub{status: 403, reads: []hostapi.StreamChunk{
		{Payload: []byte(`{"message":"The bearer token included in `)},
		{Payload: []byte(`the request is invalid"}`), Done: true},
		{Payload: eventFrame("assistantResponseEvent", `{"content":"pong"}`), Done: true},
	}})
	refreshCalls, opens, saves := 0, 0, 0
	originalHTTP, originalSave := hostHTTPDo, hostAuthSave
	hostHTTPDo = func(hostapi.HTTPRequest) (*hostapi.HTTPResponse, error) {
		refreshCalls++
		return ok(`{"accessToken":"fresh","refreshToken":"fresh-rt","expiresIn":3600}`)
	}
	hostAuthSave = func(string, []byte) error { saves++; return nil }
	t.Cleanup(func() { hostHTTPDo, hostAuthSave = originalHTTP, originalSave })
	hostHTTPDoStream = func(r hostapi.HTTPRequest) (*hostapi.HTTPStreamResponse, error) {
		opens++
		status := 403
		if opens > 1 {
			status = 200
			if got := r.Headers["Authorization"][0]; got != "Bearer fresh" {
				t.Errorf("retry used %q", got)
			}
		}
		return &hostapi.HTTPStreamResponse{StatusCode: status, StreamID: "up-1"}, nil
	}
	if env := streamCall(t, pingRequest); !env.OK {
		t.Fatalf("refresh did not recover: %+v", env.Error)
	}
	if msg := waitClosed(t, s); msg != "" {
		t.Fatal(msg)
	}
	if refreshCalls != 1 || opens != 2 || saves != 1 || len(s.ended) != 2 || !strings.Contains(strings.Join(s.emitted, ""), `"text":"pong"`) {
		t.Fatalf("refresh=%d opens=%d saves=%d ended=%v emitted=%v", refreshCalls, opens, saves, s.ended, s.emitted)
	}
}

func TestCoreRegressionNativeStopReason(t *testing.T) {
	cases := []struct {
		name, reason, want    string
		thinking, contextFull bool
	}{
		{name: "max_tokens", reason: "MAX_TOKENS", want: "max_tokens"},
		{name: "context_limit", reason: "MODEL_CONTEXT_WINDOW_EXCEEDED", want: "model_context_window_exceeded"},
		{name: "filtered", reason: "CONTENT_FILTERED", want: "refusal"},
		{name: "thinking_end_turn", reason: "END_TURN", want: "end_turn", thinking: true},
		{name: "explicit_end_over_context_estimate", reason: "END_TURN", want: "end_turn", contextFull: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conv := convertedRequest{ModelID: "test", Thinking: tc.thinking}
			events := []Event{}
			if tc.thinking {
				events = append(events, Event{Reasoning: &ReasoningContentEvent{Text: "reasoning"}})
			} else if tc.reason != "CONTENT_FILTERED" {
				events = append(events, Event{Assistant: &AssistantResponseEvent{Content: "answer"}})
			}
			if tc.contextFull {
				events = append(events, Event{ContextUsage: &ContextUsageEvent{ContextUsagePercentage: 100}})
			}
			events = append(events, Event{Metadata: &MetadataEvent{StopReason: tc.reason}})
			t.Run("nonstream", func(t *testing.T) {
				var c responseCollector
				for _, ev := range events {
					c.add(ev)
				}
				var out struct {
					StopReason string `json:"stop_reason"`
				}
				_ = json.Unmarshal(c.message(conv), &out)
				if out.StopReason != tc.want {
					t.Fatalf("got %q, want %q", out.StopReason, tc.want)
				}
			})
			t.Run("stream", func(t *testing.T) {
				s := newSSEStream(conv)
				for _, ev := range events {
					s.handle(ev)
				}
				got := ""
				for _, ev := range s.finish() {
					if ev.name == "message_delta" {
						got, _ = ev.data["delta"].(map[string]any)["stop_reason"].(string)
					}
				}
				if got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			})
		})
	}
}

func TestCoreRegressionInterleavedToolFragments(t *testing.T) {
	s := newSSEStream(convertedRequest{ModelID: "test"})
	fragments := []ToolUseEvent{
		{ToolUseID: "a", Name: "Read", Input: `{"path":`},
		{ToolUseID: "b", Name: "Read", Input: `{"path":`},
		{ToolUseID: "a", Input: `"/a"}`, Stop: true},
		{ToolUseID: "b", Input: `"/b"}`, Stop: true},
	}
	var events []sseEvent
	for _, f := range fragments {
		events = append(events, s.handle(Event{ToolUse: &f})...)
	}
	events = append(events, s.finish()...)
	ids := map[int]string{}
	inputs := map[string]string{}
	for _, ev := range events {
		if ev.name == "content_block_start" {
			block := ev.data["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				ids[ev.data["index"].(int)] = block["id"].(string)
			}
		}
		if ev.name == "content_block_delta" {
			delta := ev.data["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				inputs[ids[ev.data["index"].(int)]] += delta["partial_json"].(string)
			}
		}
	}
	for _, id := range []string{"a", "b"} {
		if inputs[id] != `{"path":"/`+id+`"}` {
			t.Errorf("tool %s incomplete after valid interleaved input: %q", id, inputs[id])
		}
	}
}

func TestCoreRegressionTrailingUserMessages(t *testing.T) {
	raw := []byte(`{"model":"claude-sonnet-4.5","messages":[
  {"role":"user","content":"run the tool"},
  {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"/a"}}]},
  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"tool output"}]},
  {"role":"user","content":"background job finished"}
 ],"tools":[{"name":"Read","description":"Read a file","input_schema":{"type":"object"}}]}`)
	conv, err := convertClaudeRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	state := conv.Payload.ConversationState
	for _, h := range state.History {
		if h.AssistantResponseMessage != nil && h.AssistantResponseMessage.Content == "OK" {
			t.Error("inserted fictitious assistant OK between tool result and trailing reminder")
		}
	}
	if got := len(state.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults); got != 1 {
		t.Errorf("current turn lost tool-result association: got %d results, want 1", got)
	}
}
