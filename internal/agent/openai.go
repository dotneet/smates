package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// OpenAI-style wire protocols: the OpenAI Responses API (responsesDialect) and the
// Chat Completions API as served by OpenRouter (chatDialect). Both use the tools of
// toolsJSON, converted once to function definitions.

type toolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type functionDef struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// responsesTools and chatTools are the tool lists of the two dialects, encoded once so
// the request prefix stays byte-identical across turns and runs (prompt caching).
var responsesTools, chatTools = func() (json.RawMessage, json.RawMessage) {
	var specs []toolSpec
	if err := json.Unmarshal(toolDefs, &specs); err != nil {
		panic("agent: invalid tool definitions: " + err.Error())
	}
	fns := make([]functionDef, len(specs))
	chats := make([]chatTool, len(specs))
	for i, s := range specs {
		var schema any
		_ = json.Unmarshal(s.InputSchema, &schema)
		fns[i] = functionDef{Type: "function", Name: s.Name, Description: s.Description, Parameters: s.InputSchema, Strict: strictSchema(schema)}
		chats[i].Type = "function"
		chats[i].Function.Name, chats[i].Function.Description, chats[i].Function.Parameters = s.Name, s.Description, s.InputSchema
	}
	return mustJSON(fns), mustJSON(chats)
}()

// strictSchema reports whether a JSON schema meets OpenAI's strict mode: every object
// lists all its properties as required and allows no others. Tools with optional
// parameters are sent without strict mode.
func strictSchema(v any) bool {
	s, ok := v.(map[string]any)
	if !ok {
		return true
	}
	if props, ok := s["properties"].(map[string]any); ok {
		required, _ := s["required"].([]any)
		if s["additionalProperties"] != false || len(required) != len(props) {
			return false
		}
		for _, p := range props {
			if !strictSchema(p) {
				return false
			}
		}
	}
	if items, ok := s["items"]; ok {
		return strictSchema(items)
	}
	return true
}

type reasoningConf struct {
	Effort string `json:"effort"`
}

func reasoningOf(p requestParams) *reasoningConf {
	if p.effort == "" {
		return nil
	}
	return &reasoningConf{Effort: p.effort}
}

// toolOutput renders a tool result; these APIs have no error flag.
func toolOutput(r toolOutcome) string {
	if r.isError {
		return "Error: " + r.content
	}
	return r.content
}

// openAIUsage converts token counts that include cached input to Usage, whose
// InputTokens exclude cache reads and writes.
func openAIUsage(input, output, cached, written int) Usage {
	return Usage{InputTokens: max(input-cached-written, 0), OutputTokens: output, CacheReadTokens: cached, CacheWriteTokens: written}
}

// openAIError decodes {"error":{"message","type","param","code"}}; OpenRouter sends a
// numeric code.
func openAIError(status int, data []byte) *APIError {
	e := &APIError{Status: status}
	var body struct {
		Error *struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Param   string          `json:"param"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) != nil || body.Error == nil || body.Error.Message == "" {
		e.Message = clip(strings.TrimSpace(string(data)), 300)
		return e
	}
	e.Type, e.Message, e.Param = body.Error.Type, body.Error.Message, body.Error.Param
	var code string
	if json.Unmarshal(body.Error.Code, &code) == nil && e.Type == "" {
		e.Type = code
	}
	return e
}

// effortRejectedByOpenAI: models without reasoning reject the effort parameter. Errors
// about the reasoning items echoed back between turns do not count.
func effortRejectedByOpenAI(e *APIError) bool {
	if e.Status != http.StatusBadRequest {
		return false
	}
	switch e.Param {
	case "reasoning.effort", "reasoning_effort", "reasoning":
		return true
	case "":
		msg := strings.ToLower(e.Message)
		return strings.Contains(msg, "reasoning.effort") || strings.Contains(msg, "reasoning_effort")
	}
	return false
}

// responsesDialect speaks the OpenAI Responses API, statelessly: nothing is stored
// on OpenAI's side, and reasoning is carried between turns encrypted.
type responsesDialect struct{}

type (
	responsesRequest struct {
		Model           string            `json:"model"`
		Instructions    string            `json:"instructions"`
		Input           []json.RawMessage `json:"input"`
		Tools           json.RawMessage   `json:"tools,omitempty"`
		ToolChoice      string            `json:"tool_choice,omitempty"`
		Text            *responsesText    `json:"text,omitempty"`
		Reasoning       *reasoningConf    `json:"reasoning,omitempty"`
		MaxOutputTokens int               `json:"max_output_tokens"`
		Store           bool              `json:"store"`
		Include         []string          `json:"include,omitempty"`
	}
	responsesText struct {
		Format responsesFormat `json:"format"`
	}
	responsesFormat struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict bool            `json:"strict"`
	}
	responsesUserMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	responsesToolOutput struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	responsesResponse struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Output []json.RawMessage `json:"output"`
		Usage  struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	responsesItem struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	}
)

func (responsesDialect) path() string { return "/responses" }

func (responsesDialect) authorize(h http.Header, key, _ string) {
	h.Set("authorization", "Bearer "+key)
}

func (responsesDialect) input(turns []turn) []json.RawMessage {
	var items []json.RawMessage
	for _, t := range turns {
		if t.model {
			items = append(items, t.assistant...)
			continue
		}
		for _, r := range t.results {
			items = append(items, mustJSON(responsesToolOutput{Type: "function_call_output", CallID: r.id, Output: toolOutput(r)}))
		}
		if t.text != "" {
			items = append(items, mustJSON(responsesUserMessage{Role: "user", Content: t.text}))
		}
	}
	return items
}

func (d responsesDialect) loopRequest(p requestParams, turns []turn) any {
	return responsesRequest{
		Model:           p.model,
		Instructions:    p.system,
		Input:           d.input(turns),
		Tools:           responsesTools,
		ToolChoice:      "auto",
		Reasoning:       reasoningOf(p),
		MaxOutputTokens: p.maxTokens,
		Include:         []string{"reasoning.encrypted_content"},
	}
}

func (d responsesDialect) explainRequest(p requestParams, brief string, schema json.RawMessage) any {
	return responsesRequest{
		Model:           p.model,
		Instructions:    p.system,
		Input:           d.input([]turn{{text: brief}}),
		Text:            &responsesText{Format: responsesFormat{Type: "json_schema", Name: "explanation", Schema: schema, Strict: true}},
		Reasoning:       reasoningOf(p),
		MaxOutputTokens: p.maxTokens,
	}
}

func (responsesDialect) decode(data []byte) (*modelReply, error) {
	var resp responsesResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != nil {
		return nil, &APIError{Status: http.StatusBadGateway, Type: resp.Error.Code, Message: resp.Error.Message}
	}
	u := resp.Usage
	r := &modelReply{
		content: resp.Output,
		stop:    resp.Status,
		usage:   openAIUsage(u.InputTokens, u.OutputTokens, u.InputTokensDetails.CachedTokens, u.InputTokensDetails.CacheWriteTokens),
	}
	if resp.Status == "incomplete" && resp.IncompleteDetails != nil {
		switch resp.IncompleteDetails.Reason {
		case "max_output_tokens":
			r.stop = stopMaxTokens
		case "content_filter":
			r.stop, r.detail = stopRefusal, "content_filter"
		}
	}
	var text strings.Builder
	for _, raw := range resp.Output {
		var it responsesItem
		if err := json.Unmarshal(raw, &it); err != nil {
			return nil, fmt.Errorf("decode response: malformed output item: %w", err)
		}
		switch it.Type {
		case "function_call":
			r.calls = append(r.calls, toolCall{ID: it.CallID, Name: it.Name, Input: arguments(it.Arguments)})
		case "message":
			for _, c := range it.Content {
				switch c.Type {
				case "output_text":
					text.WriteString(c.Text)
				case "refusal":
					r.stop = stopRefusal
				}
			}
		}
	}
	r.text = text.String()
	return r, nil
}

func (responsesDialect) apiError(status int, h http.Header, data []byte) *APIError {
	e := openAIError(status, data)
	e.RequestID = h.Get("x-request-id")
	return e
}

func (responsesDialect) effortRejected(e *APIError) bool { return effortRejectedByOpenAI(e) }

// arguments returns a function call's JSON arguments ("{}" when empty).
func arguments(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

// chatDialect speaks the Chat Completions API as served by OpenRouter.
type chatDialect struct{}

type (
	chatRequest struct {
		Model          string              `json:"model"`
		MaxTokens      int                 `json:"max_tokens"`
		CacheControl   cacheControl        `json:"cache_control"`
		Reasoning      *reasoningConf      `json:"reasoning,omitempty"`
		Messages       []json.RawMessage   `json:"messages"`
		Tools          json.RawMessage     `json:"tools,omitempty"`
		ToolChoice     string              `json:"tool_choice,omitempty"`
		ResponseFormat *chatResponseFormat `json:"response_format,omitempty"`
	}
	chatResponseFormat struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	chatText struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	chatToolMessage struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		Content    string `json:"content"`
	}
	chatToolCall struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	// chatAssistant is a model turn as sent back: reasoning_details (signatures of
	// thinking models) must be preserved for tool calling to continue.
	chatAssistant struct {
		Role             string          `json:"role"`
		Content          *string         `json:"content"`
		ToolCalls        []chatToolCall  `json:"tool_calls,omitempty"`
		ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
	}
	chatResponse struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				chatAssistant
				Refusal *string `json:"refusal"`
			} `json:"message"`
			Error *chatError `json:"error"`
		} `json:"choices"`
		Error *chatError `json:"error"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	chatError struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
)

// apiError converts an error OpenRouter reports in a 200 response (a failure of the
// upstream provider); a numeric code is the HTTP status it stands for.
func (c *chatError) apiError() *APIError {
	e := &APIError{Status: http.StatusBadGateway, Message: c.Message}
	if n, err := strconv.Atoi(string(c.Code)); err == nil && n >= 400 {
		e.Status = n
	} else {
		var s string
		if json.Unmarshal(c.Code, &s) == nil {
			e.Type = s
		}
	}
	return e
}

func (chatDialect) path() string { return "/chat/completions" }

func (chatDialect) authorize(h http.Header, key, _ string) {
	h.Set("authorization", "Bearer "+key)
	h.Set("http-referer", "https://github.com/dotneet/smates")
	h.Set("x-title", "smates")
}

func (chatDialect) messages(system string, turns []turn) []json.RawMessage {
	msgs := []json.RawMessage{mustJSON(chatText{Role: "system", Content: system})}
	for _, t := range turns {
		if t.model {
			msgs = append(msgs, t.assistant...)
			continue
		}
		for _, r := range t.results {
			msgs = append(msgs, mustJSON(chatToolMessage{Role: "tool", ToolCallID: r.id, Content: toolOutput(r)}))
		}
		if t.text != "" {
			msgs = append(msgs, mustJSON(chatText{Role: "user", Content: t.text}))
		}
	}
	return msgs
}

func (d chatDialect) loopRequest(p requestParams, turns []turn) any {
	return chatRequest{
		Model:        p.model,
		MaxTokens:    p.maxTokens,
		CacheControl: cacheControl{Type: "ephemeral"},
		Reasoning:    reasoningOf(p),
		Messages:     d.messages(p.system, turns),
		Tools:        chatTools,
		ToolChoice:   "auto",
	}
}

func (d chatDialect) explainRequest(p requestParams, brief string, schema json.RawMessage) any {
	f := &chatResponseFormat{Type: "json_schema"}
	f.JSONSchema.Name, f.JSONSchema.Strict, f.JSONSchema.Schema = "explanation", true, schema
	return chatRequest{
		Model:          p.model,
		MaxTokens:      p.maxTokens,
		CacheControl:   cacheControl{Type: "ephemeral"},
		Reasoning:      reasoningOf(p),
		Messages:       d.messages(p.system, []turn{{text: brief}}),
		ResponseFormat: f,
	}
}

func (chatDialect) decode(data []byte) (*modelReply, error) {
	var resp chatResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != nil {
		return nil, resp.Error.apiError()
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("decode response: no choices")
	}
	ch := resp.Choices[0]
	if ch.Error != nil {
		return nil, ch.Error.apiError()
	}
	u := resp.Usage
	m := ch.Message.chatAssistant
	m.Role = "assistant"
	r := &modelReply{
		content: []json.RawMessage{mustJSON(m)},
		stop:    ch.FinishReason,
		usage:   openAIUsage(u.PromptTokens, u.CompletionTokens, u.PromptTokensDetails.CachedTokens, u.PromptTokensDetails.CacheWriteTokens),
	}
	switch {
	case ch.Message.Refusal != nil && *ch.Message.Refusal != "":
		r.stop = stopRefusal
	case ch.FinishReason == "content_filter":
		r.stop, r.detail = stopRefusal, "content_filter"
	case ch.FinishReason == "length":
		r.stop = stopMaxTokens
	}
	for _, c := range m.ToolCalls {
		r.calls = append(r.calls, toolCall{ID: c.ID, Name: c.Function.Name, Input: arguments(c.Function.Arguments)})
	}
	if m.Content != nil {
		r.text = *m.Content
	}
	return r, nil
}

func (chatDialect) apiError(status int, h http.Header, data []byte) *APIError {
	e := openAIError(status, data)
	e.RequestID = h.Get("x-request-id")
	return e
}

func (chatDialect) effortRejected(e *APIError) bool { return effortRejectedByOpenAI(e) }
