package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ModelGroup is the subscription group inferred from a model identifier.
type ModelGroup string

const (
	GroupGPT       ModelGroup = "GPT"
	GroupAnthropic ModelGroup = "ANTHROPIC"
	GroupChina     ModelGroup = "CHINA"
	GroupUnknown   ModelGroup = "UNKNOWN"
)

// GroupClassify classifies one model ID without guessing. Matching is
// case-insensitive because relays do not consistently preserve model casing.
func GroupClassify(modelID string) ModelGroup {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.HasPrefix(modelID, "gpt-"):
		return GroupGPT
	case strings.HasPrefix(modelID, "claude-"):
		return GroupAnthropic
	case hasAnyPrefix(modelID, "gemma", "glm", "qwen", "kimi", "minimax"):
		return GroupChina
	default:
		return GroupUnknown
	}
}

func hasAnyPrefix(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// ModelsResult includes the bounded inventory and a deterministic grouping of
// every returned model, including unknown identifiers.
type ModelsResult struct {
	Result
	Models []string
	Groups map[ModelGroup][]string
}

// ModelsProbe performs GET /v1/models.
type ModelsProbe struct{ client *Client }

func NewModelsProbe(client *Client) *ModelsProbe { return &ModelsProbe{client: client} }

func (probe *ModelsProbe) Probe(ctx context.Context) (ModelsResult, error) {
	if probe == nil || probe.client == nil {
		return ModelsResult{}, errors.New("probe: models client is required")
	}
	var models []string
	result, _, err := probe.client.execute(ctx, requestSpec{
		protocol: ProtocolModels,
		method:   http.MethodGet,
		path:     "/v1/models",
		validate: func(body []byte) bool {
			var valid bool
			models, valid = decodeModels(body)
			return valid
		},
	})
	if err != nil {
		return ModelsResult{}, err
	}
	groups := make(map[ModelGroup][]string)
	if result.Status == StatusConfirmed {
		for _, model := range models {
			group := GroupClassify(model)
			groups[group] = append(groups[group], model)
		}
		for group := range groups {
			sort.Strings(groups[group])
		}
	}
	return ModelsResult{Result: result, Models: models, Groups: groups}, nil
}

// ResponsesProbe performs a minimal non-streaming POST /v1/responses. The
// owner-selected model is supplied for each call and is never guessed.
type ResponsesProbe struct{ client *Client }

func NewResponsesProbe(client *Client) *ResponsesProbe { return &ResponsesProbe{client: client} }

func (probe *ResponsesProbe) Probe(ctx context.Context, model string) (Result, error) {
	if probe == nil || probe.client == nil {
		return Result{}, errors.New("probe: responses client is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return Result{}, errors.New("probe: responses model is required")
	}
	body, err := json.Marshal(struct {
		Model  string `json:"model"`
		Input  string `json:"input"`
		Stream bool   `json:"stream"`
	}{Model: model, Input: "ping", Stream: false})
	if err != nil {
		return Result{}, errors.New("probe: could not encode responses request")
	}
	result, _, err := probe.client.execute(ctx, requestSpec{
		protocol: ProtocolResponses,
		method:   http.MethodPost,
		path:     "/v1/responses",
		body:     body,
		validate: validateResponses,
	})
	return result, err
}

// HTTPProber implements the skeleton's protocol-dispatch interface with an
// owner-selected runtime model. Call ModelsProbe directly when its inventory
// and grouping details are required.
type HTTPProber struct {
	client *Client
	model  string
}

var _ Prober = (*HTTPProber)(nil)

func NewHTTPProber(client *Client, model string) (*HTTPProber, error) {
	if client == nil {
		return nil, errors.New("probe: client is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, errors.New("probe: runtime model is required")
	}
	return &HTTPProber{client: client, model: model}, nil
}

func (probe *HTTPProber) Probe(ctx context.Context, protocol Protocol) (Result, error) {
	if probe == nil || probe.client == nil {
		return Result{}, errors.New("probe: client is required")
	}
	switch protocol {
	case ProtocolModels:
		result, err := NewModelsProbe(probe.client).Probe(ctx)
		return result.Result, err
	case ProtocolResponses:
		return NewResponsesProbe(probe.client).Probe(ctx, probe.model)
	case ProtocolMessages:
		return NewMessagesProbe(probe.client).Probe(ctx, probe.model)
	default:
		return Result{}, errors.New("probe: unsupported protocol")
	}
}

// MessagesProbe performs a minimal non-streaming POST /v1/messages.
type MessagesProbe struct{ client *Client }

func NewMessagesProbe(client *Client) *MessagesProbe { return &MessagesProbe{client: client} }

func (probe *MessagesProbe) Probe(ctx context.Context, model string) (Result, error) {
	if probe == nil || probe.client == nil {
		return Result{}, errors.New("probe: messages client is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return Result{}, errors.New("probe: messages model is required")
	}
	body, err := json.Marshal(struct {
		Model     string `json:"model"`
		Messages  []any  `json:"messages"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
	}{
		Model: model,
		Messages: []any{map[string]string{
			"role":    "user",
			"content": "ping",
		}},
		MaxTokens: 1,
		Stream:    false,
	})
	if err != nil {
		return Result{}, errors.New("probe: could not encode messages request")
	}
	headers := make(http.Header)
	headers.Set("Anthropic-Version", "2023-06-01")
	result, _, err := probe.client.execute(ctx, requestSpec{
		protocol: ProtocolMessages,
		method:   http.MethodPost,
		path:     "/v1/messages",
		body:     body,
		headers:  headers,
		validate: validateMessages,
	})
	return result, err
}

func decodeModels(body []byte) ([]string, bool) {
	var envelope struct {
		Object string          `json:"object"`
		Data   json.RawMessage `json:"data"`
	}
	if !decodeOne(body, &envelope) || envelope.Object != "list" || !isJSONArray(envelope.Data) {
		return nil, false
	}
	var entries []struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	if err := json.Unmarshal(envelope.Data, &entries); err != nil || len(entries) == 0 {
		return nil, false
	}
	models := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.ID)
		if id == "" || entry.Object != "model" {
			return nil, false
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	return models, len(models) > 0
}

func validateResponses(body []byte) bool {
	var envelope struct {
		ID     string          `json:"id"`
		Object string          `json:"object"`
		Status string          `json:"status"`
		Model  string          `json:"model"`
		Error  json.RawMessage `json:"error"`
		Output json.RawMessage `json:"output"`
	}
	if !decodeOne(body, &envelope) || strings.TrimSpace(envelope.ID) == "" ||
		envelope.Object != "response" || envelope.Status != "completed" ||
		strings.TrimSpace(envelope.Model) == "" || !isNullOrMissing(envelope.Error) {
		return false
	}
	var output []json.RawMessage
	if !decodeJSONArray(envelope.Output, &output) || len(output) == 0 {
		return false
	}
	messageSeen := false
	for _, rawItem := range output {
		var item struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Status  string          `json:"status"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			Summary json.RawMessage `json:"summary"`
		}
		if !decodeOne(rawItem, &item) || strings.TrimSpace(item.ID) == "" || item.Type == "" {
			return false
		}
		if item.Type == "reasoning" {
			if !isJSONArray(item.Summary) {
				return false
			}
			continue
		}
		if item.Type != "message" || item.Status != "completed" ||
			item.Role != "assistant" || !validResponsesContent(item.Content) {
			return false
		}
		messageSeen = true
	}
	return messageSeen
}

func validateMessages(body []byte) bool {
	var envelope struct {
		ID         string          `json:"id"`
		Type       string          `json:"type"`
		Role       string          `json:"role"`
		Model      string          `json:"model"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	}
	if !decodeOne(body, &envelope) || strings.TrimSpace(envelope.ID) == "" ||
		envelope.Type != "message" || envelope.Role != "assistant" ||
		strings.TrimSpace(envelope.Model) == "" || envelope.StopReason == "" {
		return false
	}
	var content []json.RawMessage
	if !decodeJSONArray(envelope.Content, &content) || len(content) == 0 {
		return false
	}
	textSeen := false
	for _, rawItem := range content {
		var item struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		}
		if !decodeOne(rawItem, &item) || item.Type == "" {
			return false
		}
		if item.Type != "text" || item.Text == nil {
			return false
		}
		textSeen = true
	}
	return textSeen
}

func validResponsesContent(raw json.RawMessage) bool {
	var content []json.RawMessage
	if !decodeJSONArray(raw, &content) || len(content) == 0 {
		return false
	}
	for _, rawItem := range content {
		var item struct {
			Type    string  `json:"type"`
			Text    *string `json:"text"`
			Refusal *string `json:"refusal"`
		}
		if !decodeOne(rawItem, &item) {
			return false
		}
		switch item.Type {
		case "output_text":
			if item.Text == nil {
				return false
			}
		case "refusal":
			if item.Refusal == nil {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func decodeOne(body []byte, destination any) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(destination); err != nil {
		return false
	}
	var extra any
	return errors.Is(decoder.Decode(&extra), io.EOF)
}

func isJSONArray(raw json.RawMessage) bool {
	var values []json.RawMessage
	return decodeJSONArray(raw, &values)
}

func decodeJSONArray(raw json.RawMessage, destination any) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return false
	}
	return json.Unmarshal(trimmed, destination) == nil
}

func isNullOrMissing(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}
