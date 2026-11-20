// Package fireworks adapts Bobik's model requests to Fireworks Chat Completions.
package fireworks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const (
	defaultEndpoint  = "https://api.fireworks.ai/inference/v1/chat/completions"
	maxResponseBytes = 4 << 20
)

// Client uses the Fireworks Chat Completions endpoint unless Endpoint is a
// non-empty full URL. A nil HTTP uses http.DefaultClient.
type Client struct {
	APIKey   string
	Endpoint string
	HTTP     *http.Client
}

type Request struct {
	Model     string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
}

type Message struct {
	Role, Content, Reasoning, ToolCallID string
	ToolCalls                            []Call
	ImageURL                             string
}

type Call struct {
	ID, Name, Arguments string
}

type Tool struct {
	Name, Description string
	Parameters        json.RawMessage
}

type Response struct {
	Message      Message
	FinishReason string
	Usage        Usage
}

type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

type wireRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	Tools     []wireTool    `json:"tools,omitempty"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Stream    bool          `json:"stream"`
}

type wireMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	Reasoning  string     `json:"reasoning_content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     *string   `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type wireCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string         `json:"type"`
	Function wireDefinition `json:"function"`
}

type wireDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      *struct {
			Role      string     `json:"role"`
			Content   string     `json:"content"`
			Reasoning string     `json:"reasoning_content"`
			ToolCalls []wireCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		Prompt     int  `json:"prompt_tokens"`
		Completion int  `json:"completion_tokens"`
		Total      *int `json:"total_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

func encodeRequest(req Request) ([]byte, error) {
	wire := wireRequest{
		Model:     req.Model,
		Messages:  make([]wireMessage, 0, len(req.Messages)),
		MaxTokens: req.MaxTokens,
	}
	for _, msg := range req.Messages {
		m := wireMessage{Role: msg.Role, Content: msg.Content, Reasoning: msg.Reasoning, ToolCallID: msg.ToolCallID}
		if msg.ImageURL != "" {
			m.Content = []contentPart{
				{Type: "text", Text: &msg.Content},
				{Type: "image_url", ImageURL: &imageURL{URL: msg.ImageURL}},
			}
		} else if msg.Role == "assistant" && msg.Content == "" && len(msg.ToolCalls) != 0 {
			m.Content = nil
		}
		for _, call := range msg.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, wireCall{
				ID: call.ID, Type: "function", Function: wireFunction{Name: call.Name, Arguments: call.Arguments},
			})
		}
		wire.Messages = append(wire.Messages, m)
	}
	for _, tool := range req.Tools {
		wire.Tools = append(wire.Tools, wireTool{
			Type:     "function",
			Function: wireDefinition{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters},
		})
	}
	return json.Marshal(wire)
}

// Complete makes a single, non-streaming completion request. Errors are safe
// to audit: they do not carry HTTP URLs, error bodies, or transport internals.
// A usable usage report can accompany an error (e.g. a missing choice).
func (c *Client) Complete(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	body, err := encodeRequest(req)
	if err != nil {
		return Response{}, errors.New("could not encode Fireworks request")
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, errors.New("invalid Fireworks endpoint")
	}
	request.GetBody = nil // This POST must not be replayed after a failure.
	request.Header.Set("Authorization", "Bearer "+c.APIKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	// Never forward a prompt, image, or API key to a redirect target.
	httpClient := *client
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	reply, err := httpClient.Do(request)
	if err != nil {
		return Response{}, safeError(ctx, err, "Fireworks request failed")
	}
	defer reply.Body.Close()
	if reply.StatusCode < 200 || reply.StatusCode >= 300 {
		return Response{}, fmt.Errorf("Fireworks request failed (HTTP %d)", reply.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(reply.Body, maxResponseBytes+1))
	if err != nil {
		return Response{}, safeError(ctx, err, "could not read Fireworks response")
	}
	if len(data) > maxResponseBytes {
		return Response{}, errors.New("Fireworks response exceeds the 4 MiB limit")
	}
	var wire wireResponse
	if err := json.Unmarshal(data, &wire); err != nil {
		return Response{}, errors.New("invalid Fireworks response")
	}
	resp := Response{Usage: Usage{InputTokens: wire.Usage.Prompt, OutputTokens: wire.Usage.Completion}}
	if wire.Usage.Total != nil {
		resp.Usage.TotalTokens = *wire.Usage.Total
	} else {
		resp.Usage.TotalTokens = resp.Usage.InputTokens + resp.Usage.OutputTokens
	}
	if len(wire.Error) != 0 && string(wire.Error) != "null" {
		return resp, errors.New("Fireworks returned a provider error")
	}
	if len(wire.Choices) == 0 || wire.Choices[0].Message == nil {
		return resp, errors.New("Fireworks response has no completion message")
	}
	choice := wire.Choices[0]
	msg := choice.Message
	resp.FinishReason = choice.FinishReason
	// A provider can report an application error even on HTTP 200. Keep its
	// finish reason and usage, but not its potentially sensitive message.
	if resp.FinishReason == "error" {
		resp.Message.Role = "assistant"
		return resp, nil
	}
	resp.Message = Message{Role: msg.Role, Content: msg.Content, Reasoning: msg.Reasoning}
	if resp.Message.Role == "" {
		resp.Message.Role = "assistant"
	}
	for _, call := range msg.ToolCalls {
		resp.Message.ToolCalls = append(resp.Message.ToolCalls, Call{
			ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments,
		})
	}
	return resp, nil
}

func safeError(ctx context.Context, err error, message string) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", message, ctxErr)
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, cause) {
			return fmt.Errorf("%s: %w", message, cause)
		}
	}
	return errors.New(message)
}
