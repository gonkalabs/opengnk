// Package toolsim rewrites OpenAI-style tool-call requests into plain
// chat-completion prompts and converts the model's JSON response back
// into the proper tool_calls format. This allows tool calling to work
// even when the upstream inference server doesn't support it natively.
package toolsim

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// ---------- OpenAI request/response types ----------

// ChatRequest is a minimal representation of the OpenAI chat request.
type ChatRequest struct {
	Model      string          `json:"model"`
	Messages   []Message       `json:"messages"`
	Tools      []Tool          `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	Stream     bool            `json:"stream,omitempty"`
	// Preserve everything else.
	Extra map[string]json.RawMessage `json:"-"`
}

// Message is an OpenAI chat message.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // can be string or null
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCallMsg   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// ToolCallMsg is a tool_call inside an assistant message.
type ToolCallMsg struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the function part of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

// Tool is an OpenAI tool definition.
type Tool struct {
	Type     string       `json:"type"`
	Function FunctionDef  `json:"function"`
}

// FunctionDef is the definition of a function tool.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ---------- public API ----------

// NeedsSimulation returns true if the request contains tools that need
// to be simulated.
func NeedsSimulation(body []byte) bool {
	var peek struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return false
	}
	return len(peek.Tools) > 0
}

// RewriteRequest takes the original request body (with tools) and returns
// a new body with the tools removed and a system prompt injected that
// instructs the model to respond with tool calls in JSON.
// It also returns the original tools so we can parse the response later.
func RewriteRequest(body []byte) (newBody []byte, tools []Tool, wasStream bool, err error) {
	// Parse the full request preserving unknown fields.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, false, fmt.Errorf("toolsim: unmarshal request: %w", err)
	}

	// Extract tools.
	var toolList []Tool
	if t, ok := raw["tools"]; ok {
		if err := json.Unmarshal(t, &toolList); err != nil {
			return nil, nil, false, fmt.Errorf("toolsim: unmarshal tools: %w", err)
		}
	}
	if len(toolList) == 0 {
		return body, nil, false, nil // nothing to simulate
	}

	// Extract messages.
	var messages []Message
	if m, ok := raw["messages"]; ok {
		if err := json.Unmarshal(m, &messages); err != nil {
			return nil, nil, false, fmt.Errorf("toolsim: unmarshal messages: %w", err)
		}
	}

	// Check stream flag.
	var stream bool
	if s, ok := raw["stream"]; ok {
		_ = json.Unmarshal(s, &stream)
	}

	// Build the tool description for the system prompt.
	toolDesc := buildToolDescription(toolList)

	// Determine tool_choice hint.
	choiceHint := ""
	if tc, ok := raw["tool_choice"]; ok {
		choiceHint = parseToolChoice(tc, toolList)
	}

	// Build the system instruction.
	sysPrompt := buildSystemPrompt(toolDesc, choiceHint)

	// Prepend our system message (or merge with existing system message).
	messages = injectSystemPrompt(messages, sysPrompt)

	// Re-serialize messages.
	msgBytes, err := json.Marshal(messages)
	if err != nil {
		return nil, nil, false, fmt.Errorf("toolsim: marshal messages: %w", err)
	}
	raw["messages"] = msgBytes

	// Upstream nodes don't support tools; strip them before forwarding.
	delete(raw, "tools")
	delete(raw, "tool_choice")

	// Force non-streaming for tool simulation (we need the full response to parse).
	raw["stream"] = json.RawMessage("false")

	newBody, err = json.Marshal(raw)
	if err != nil {
		return nil, nil, false, fmt.Errorf("toolsim: marshal request: %w", err)
	}

	slog.Info("toolsim: rewrote request", "tools", len(toolList), "originalStream", stream)
	return newBody, toolList, stream, nil
}

// ParseResponse takes the upstream response body and tries to extract
// tool calls from the assistant's content. Returns a rewritten response
// with proper tool_calls format, or the original response if no tool
// calls were found.
func ParseResponse(respBody []byte, tools []Tool, originalModel string) []byte {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return respBody
	}

	var choices []map[string]json.RawMessage
	if c, ok := resp["choices"]; ok {
		if err := json.Unmarshal(c, &choices); err != nil || len(choices) == 0 {
			return respBody
		}
	}

	// Get the message from first choice.
	var msg map[string]json.RawMessage
	if m, ok := choices[0]["message"]; ok {
		if err := json.Unmarshal(m, &msg); err != nil {
			return respBody
		}
	}

	// Extract content string.
	var content string
	if c, ok := msg["content"]; ok {
		if err := json.Unmarshal(c, &content); err != nil {
			return respBody
		}
	}

	// Try to extract tool calls from the content.
	toolCalls := extractToolCalls(content, tools)
	if len(toolCalls) == 0 {
		return respBody
	}

	slog.Info("toolsim: parsed tool calls from response", "count", len(toolCalls))

	// Build proper OpenAI tool_calls response.
	toolCallMsgs := make([]ToolCallMsg, len(toolCalls))
	for i, tc := range toolCalls {
		toolCallMsgs[i] = ToolCallMsg{
			ID:   generateToolCallID(),
			Type: "function",
			Function: FunctionCall{
				Name:      tc.Name,
				Arguments: tc.Arguments,
			},
		}
	}

	// Rewrite the message.
	msg["role"] = json.RawMessage(`"assistant"`)
	msg["content"] = json.RawMessage("null")
	tcBytes, _ := json.Marshal(toolCallMsgs)
	msg["tool_calls"] = json.RawMessage(tcBytes)

	// Rewrite finish_reason.
	choices[0]["message"], _ = json.Marshal(msg)
	choices[0]["finish_reason"] = json.RawMessage(`"tool_calls"`)

	resp["choices"], _ = json.Marshal(choices)

	out, err := json.Marshal(resp)
	if err != nil {
		return respBody
	}
	return out
}

// ---------- internals ----------

type parsedToolCall struct {
	Name      string
	Arguments string // JSON string
}

func buildToolDescription(tools []Tool) string {
	var sb strings.Builder
	for i, t := range tools {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(fmt.Sprintf("### Function %d: `%s`\n", i+1, t.Function.Name))
		if t.Function.Description != "" {
			sb.WriteString(fmt.Sprintf("Description: %s\n", t.Function.Description))
		}
		if len(t.Function.Parameters) > 0 && string(t.Function.Parameters) != "null" {
			sb.WriteString(fmt.Sprintf("Parameters (JSON Schema):\n```json\n%s\n```", string(t.Function.Parameters)))
		}
	}
	return sb.String()
}

func parseToolChoice(raw json.RawMessage, tools []Tool) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "none":
			return "Do NOT call any tools. Respond normally."
		case "required":
			return "You MUST call at least one tool."
		default:
			return "" // "auto": model decides on its own
		}
	}
	// Could be {"type": "function", "function": {"name": "..."}}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Function.Name != "" {
		return fmt.Sprintf("You MUST call the `%s` function.", obj.Function.Name)
	}
	return ""
}

func buildSystemPrompt(toolDesc, choiceHint string) string {
	var sb strings.Builder
	sb.WriteString("You have access to the following tools/functions:\n\n")
	sb.WriteString(toolDesc)
	sb.WriteString("\n\n")
	sb.WriteString("## Instructions\n")
	sb.WriteString("If the user's request can be answered by calling one or more of these tools, respond with ONLY a JSON array of tool calls in this exact format:\n")
	sb.WriteString("```json\n")
	sb.WriteString(`[{"name": "function_name", "arguments": {"param1": "value1"}}]`)
	sb.WriteString("\n```\n\n")
	sb.WriteString("Rules:\n")
	sb.WriteString("- Output ONLY the raw JSON array, no markdown code fences, no explanation.\n")
	sb.WriteString("- `arguments` must be a JSON object matching the parameter schema.\n")
	sb.WriteString("- You may call multiple tools by including multiple objects in the array.\n")
	sb.WriteString("- If you do NOT need to call any tool, respond normally with plain text.\n")
	if choiceHint != "" {
		sb.WriteString(fmt.Sprintf("\nIMPORTANT: %s\n", choiceHint))
	}
	return sb.String()
}

func injectSystemPrompt(messages []Message, sysPrompt string) []Message {
	sysContent, _ := json.Marshal(sysPrompt)
	sysMsg := Message{
		Role:    "system",
		Content: sysContent,
	}

	// If the first message is already a system message, prepend ours before it.
	result := make([]Message, 0, len(messages)+1)
	result = append(result, sysMsg)
	result = append(result, messages...)
	return result
}

func extractToolCalls(content string, tools []Tool) []parsedToolCall {
	content = strings.TrimSpace(content)

	// Strip markdown code fences if model wrapped the JSON.
	content = stripCodeFences(content)
	content = strings.TrimSpace(content)

	// Build a set of valid function names for validation.
	validNames := make(map[string]bool, len(tools))
	for _, t := range tools {
		validNames[t.Function.Name] = true
	}

	// Try to parse as a JSON array of tool calls.
	var calls []struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(content), &calls); err == nil && len(calls) > 0 {
		var result []parsedToolCall
		for _, c := range calls {
			if !validNames[c.Name] {
				continue
			}
			args := string(c.Arguments)
			if args == "" || args == "null" {
				args = "{}"
			}
			result = append(result, parsedToolCall{
				Name:      c.Name,
				Arguments: args,
			})
		}
		if len(result) > 0 {
			return result
		}
	}

	// Try to find JSON array embedded in the text.
	start := strings.Index(content, "[")
	end := strings.LastIndex(content, "]")
	if start >= 0 && end > start {
		substring := content[start : end+1]
		if err := json.Unmarshal([]byte(substring), &calls); err == nil && len(calls) > 0 {
			var result []parsedToolCall
			for _, c := range calls {
				if !validNames[c.Name] {
					continue
				}
				args := string(c.Arguments)
				if args == "" || args == "null" {
					args = "{}"
				}
				result = append(result, parsedToolCall{
					Name:      c.Name,
					Arguments: args,
				})
			}
			return result
		}
	}

	// Try single object (model returned one call without array).
	var single struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(content), &single); err == nil && single.Name != "" && validNames[single.Name] {
		args := string(single.Arguments)
		if args == "" || args == "null" {
			args = "{}"
		}
		return []parsedToolCall{{Name: single.Name, Arguments: args}}
	}

	return nil
}

func stripCodeFences(s string) string {
	// Remove ```json ... ``` or ``` ... ```
	if strings.HasPrefix(s, "```") {
		lines := strings.SplitN(s, "\n", 2)
		if len(lines) == 2 {
			s = lines[1]
		}
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
	}
	return s
}

func generateToolCallID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "call_" + hex.EncodeToString(b)
}
