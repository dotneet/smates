package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const anthropicVersion = "2023-06-01"

// anthropicDialect speaks the Anthropic Messages API.
type anthropicDialect struct{}

// Wire types. Content blocks are kept as raw JSON so assistant turns (including
// thinking blocks and their signatures) are echoed back byte for byte.
type (
	apiMessage struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	apiRequest struct {
		Model        string            `json:"model"`
		MaxTokens    int               `json:"max_tokens"`
		OutputConfig *outputConfig     `json:"output_config,omitempty"`
		CacheControl cacheControl      `json:"cache_control"`
		System       string            `json:"system"`
		Tools        json.RawMessage   `json:"tools"`
		ToolChoice   map[string]string `json:"tool_choice"`
		Messages     []apiMessage      `json:"messages"`
	}
	outputConfig struct {
		Effort string `json:"effort"`
	}
	cacheControl struct {
		Type string `json:"type"`
	}
	explainBody struct {
		Model        string            `json:"model"`
		MaxTokens    int               `json:"max_tokens"`
		CacheControl cacheControl      `json:"cache_control"`
		System       string            `json:"system"`
		OutputConfig explainOutputConf `json:"output_config"`
		Messages     []apiMessage      `json:"messages"`
	}
	explainOutputConf struct {
		Effort string        `json:"effort,omitempty"`
		Format explainFormat `json:"format"`
	}
	explainFormat struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	}
	apiResponse struct {
		ID          string            `json:"id"`
		Content     []json.RawMessage `json:"content"`
		StopReason  string            `json:"stop_reason"`
		StopDetails *struct {
			Category    string `json:"category"`
			Explanation string `json:"explanation"`
		} `json:"stop_details"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	// blockHeader is the part of a content block the loop inspects.
	blockHeader struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
		Text  string          `json:"text"`
	}
	toolResult struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		Content   string `json:"content"`
		IsError   bool   `json:"is_error,omitempty"`
	}
	textBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
)

func (anthropicDialect) path() string { return "/v1/messages" }

func (anthropicDialect) authorize(h http.Header, key, token string) {
	h.Set("anthropic-version", anthropicVersion)
	if key != "" {
		h.Set("x-api-key", key)
	} else {
		h.Set("authorization", "Bearer "+token)
	}
}

func (anthropicDialect) messages(turns []turn) []apiMessage {
	msgs := make([]apiMessage, 0, len(turns))
	for _, t := range turns {
		if t.model {
			msgs = append(msgs, apiMessage{Role: "assistant", Content: t.assistant})
			continue
		}
		var content []json.RawMessage
		for _, r := range t.results {
			content = append(content, mustJSON(toolResult{Type: "tool_result", ToolUseID: r.id, Content: r.content, IsError: r.isError}))
		}
		if t.text != "" {
			content = append(content, mustJSON(textBlock{"text", t.text}))
		}
		msgs = append(msgs, apiMessage{Role: "user", Content: content})
	}
	return msgs
}

func (d anthropicDialect) loopRequest(p requestParams, turns []turn) any {
	r := apiRequest{
		Model:        p.model,
		MaxTokens:    p.maxTokens,
		CacheControl: cacheControl{Type: "ephemeral"},
		System:       p.system,
		Tools:        toolDefs,
		ToolChoice:   map[string]string{"type": "auto"},
		Messages:     d.messages(turns),
	}
	if p.effort != "" {
		r.OutputConfig = &outputConfig{Effort: p.effort}
	}
	return r
}

func (d anthropicDialect) explainRequest(p requestParams, brief string, schema json.RawMessage) any {
	return explainBody{
		Model:        p.model,
		MaxTokens:    p.maxTokens,
		CacheControl: cacheControl{Type: "ephemeral"},
		System:       p.system,
		OutputConfig: explainOutputConf{Effort: p.effort, Format: explainFormat{Type: "json_schema", Schema: schema}},
		Messages:     d.messages([]turn{{text: brief}}),
	}
}

func (anthropicDialect) decode(data []byte) (*modelReply, error) {
	var resp apiResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	r := &modelReply{
		content: resp.Content,
		stop:    resp.StopReason,
		usage: Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens,
			CacheReadTokens: resp.Usage.CacheReadInputTokens, CacheWriteTokens: resp.Usage.CacheCreationInputTokens},
	}
	if resp.StopDetails != nil {
		r.detail = resp.StopDetails.Category
	}
	var text strings.Builder
	for _, raw := range resp.Content {
		var h blockHeader
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, fmt.Errorf("decode response: malformed content block: %w", err)
		}
		switch h.Type {
		case "tool_use":
			r.calls = append(r.calls, toolCall{ID: h.ID, Name: h.Name, Input: h.Input})
		case "text":
			text.WriteString(h.Text)
		}
	}
	r.text = text.String()
	return r, nil
}

func (anthropicDialect) apiError(status int, h http.Header, data []byte) *APIError {
	e := &APIError{Status: status, RequestID: h.Get("request-id")}
	var body struct {
		Error struct{ Type, Message string }
	}
	if json.Unmarshal(data, &body) == nil && body.Error.Type != "" {
		e.Type, e.Message = body.Error.Type, body.Error.Message
	} else {
		e.Message = clip(strings.TrimSpace(string(data)), 300)
	}
	return e
}

// effortRejected: the Messages API's effort setting is left to the user's config.
func (anthropicDialect) effortRejected(*APIError) bool { return false }
