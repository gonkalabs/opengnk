package api

import (
	"encoding/json"
	"testing"
)

func TestNormalizeMessageContentConvertsBlankAssistantToolCallContentToNull(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "call test tool"},
			{
				"role": "assistant",
				"content": " ",
				"tool_calls": [
					{
						"id": "call_1",
						"type": "function",
						"function": {"name": "test_tool", "arguments": "{}"}
					}
				]
			},
			{"role": "tool", "tool_call_id": "call_1", "content": "ok"}
		]
	}`)

	out, err := normalizeMessageContent(body)
	if err != nil {
		t.Fatalf("normalizeMessageContent() error = %v", err)
	}

	var req struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal normalized body: %v", err)
	}

	if string(req.Messages[1]["content"]) != "null" {
		t.Fatalf("assistant tool-call content = %s, want null", req.Messages[1]["content"])
	}
}

func TestNormalizeMessageContentKeepsPlainEmptyAssistantContent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":""}]}`)

	out, err := normalizeMessageContent(body)
	if err != nil {
		t.Fatalf("normalizeMessageContent() error = %v", err)
	}

	var req struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal normalized body: %v", err)
	}

	if string(req.Messages[0]["content"]) != `""` {
		t.Fatalf("plain assistant content = %s, want empty string", req.Messages[0]["content"])
	}
}
