package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// Client communicates with the Command Code Provider API and falls back to native Gemini.
type Client struct {
	HTTPClient        *http.Client
	CommandCodeAPIKey string
	CommandCodeModel  string
	FallbackAPIKey    string
	// Endpoint overrides the Command Code chat-completions URL. Empty means production;
	// tests point it at an httptest server.
	Endpoint string
}

// commandCodeURL is the production chat-completions endpoint.
const commandCodeURL = "https://api.commandcode.ai/provider/v1/chat/completions"

// endpoint is the URL to post to: the override when set, else production.
func (c *Client) endpoint() string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	return commandCodeURL
}

// model returns the Command Code model id to use, defaulting to the
// historical MiniMax model when unset.
func (c *Client) model() string {
	if c.CommandCodeModel != "" {
		return c.CommandCodeModel
	}
	return "MiniMaxAI/MiniMax-M3"
}

// Caller is the interface for making API calls.
type Caller interface {
	Call(prompt string, schema *Schema) (string, bool, error)
	CallGrouping(prompt string) (string, bool, error)
	// CallStreamContext streams the reply: see the method on Client for the contract.
	CallStreamContext(ctx context.Context, prompt string, schema *Schema, onDelta func(string) error) (string, error)
}

// ---------- request / response types ----------

type Schema struct {
	Type                 string            `json:"type"`
	Properties           map[string]Schema `json:"properties,omitempty"`
	Required             []string          `json:"required,omitempty"`
	AdditionalProperties bool              `json:"additionalProperties"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema *ResponseSchema `json:"json_schema,omitempty"`
}

type ResponseSchema struct {
	Name   string  `json:"name"`
	Strict bool    `json:"strict"`
	Schema *Schema `json:"schema"`
}

type request struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
}

// streamChunk is one SSE frame of a streamed completion: the incremental delta only.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

type response struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// fixSchema recursively lowers the type strings and sets additionalProperties to false.
func fixSchema(s *Schema) *Schema {
	if s == nil {
		return nil
	}
	fixed := &Schema{
		Type:                 strings.ToLower(s.Type),
		Required:             s.Required,
		AdditionalProperties: false,
	}
	if len(s.Properties) > 0 {
		fixed.Properties = make(map[string]Schema)
		for k, v := range s.Properties {
			fixed.Properties[k] = *fixSchema(&v)
		}
	}
	return fixed
}

// ---------- native gemini request types ----------

type nativePart struct {
	Text string `json:"text"`
}

type nativeContent struct {
	Parts []nativePart `json:"parts"`
	Role  string       `json:"role,omitempty"`
}

type nativeGenConfig struct {
	Temperature      float64 `json:"temperature,omitempty"`
	ResponseMimeType string  `json:"responseMimeType,omitempty"`
}

type nativeRequest struct {
	Contents         []nativeContent  `json:"contents"`
	GenerationConfig *nativeGenConfig `json:"generationConfig,omitempty"`
}

// ---------- public methods ----------

func (c *Client) Call(prompt string, schema *Schema) (string, bool, error) {
	return c.CallContext(context.Background(), prompt, schema)
}

func (c *Client) CallContext(ctx context.Context, prompt string, schema *Schema) (string, bool, error) {
	resp, err := c.CallWithModelContext(ctx, prompt, c.model(), 65536, schema)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		log.Printf("[LLM] CommandCode call failed: %v. Falling back to native Gemini...", err)
		fallbackResp, fErr := c.CallNativeFallbackContext(ctx, prompt, schema)
		if fErr != nil {
			return "", false, fmt.Errorf("CommandCode error: %v, Fallback error: %w", err, fErr)
		}
		return fallbackResp, true, nil
	}
	return resp, false, nil
}

func (c *Client) CallGrouping(prompt string) (string, bool, error) {
	resp, err := c.CallWithModel(prompt, c.model(), 65536, nil)
	if err != nil {
		log.Printf("[LLM] CommandCode grouping failed: %v. Falling back to native Gemini...", err)
		fallbackResp, fErr := c.CallNativeFallback(prompt, nil)
		if fErr != nil {
			return "", false, fmt.Errorf("CommandCode error: %v, Fallback error: %w", err, fErr)
		}
		return fallbackResp, true, nil
	}
	return resp, false, nil
}

func (c *Client) CallNativeFallback(prompt string, schema *Schema) (string, error) {
	return c.CallNativeFallbackContext(context.Background(), prompt, schema)
}

func (c *Client) CallNativeFallbackContext(ctx context.Context, prompt string, schema *Schema) (string, error) {
	if c.FallbackAPIKey == "" {
		return "", fmt.Errorf("GEMINI_API_KEY not configured")
	}

	reqBody := nativeRequest{
		Contents: []nativeContent{
			{Parts: []nativePart{{Text: prompt}}},
		},
		GenerationConfig: &nativeGenConfig{
			Temperature:      0.2,
			ResponseMimeType: "application/json",
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	url := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key=" + c.FallbackAPIKey
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("native Gemini API error %d: %s", resp.StatusCode, string(respBody))
	}

	var gr struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(respBody, &gr); err != nil {
		return "", err
	}

	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty candidates from Gemini")
	}

	return strings.TrimSpace(gr.Candidates[0].Content.Parts[0].Text), nil
}

func (c *Client) CallWithModel(prompt, model string, maxTokens int, schema *Schema) (string, error) {
	return c.CallWithModelContext(context.Background(), prompt, model, maxTokens, schema)
}

func (c *Client) CallWithModelContext(ctx context.Context, prompt, model string, maxTokens int, schema *Schema) (string, error) {
	if c.CommandCodeAPIKey == "" {
		return "", fmt.Errorf("COMMAND_CODE_API_KEY not configured")
	}

	req := request{
		Model: model,
		Messages: []Message{
			{Role: "user", Content: prompt},
		},
		Temperature: 0.2,
		MaxTokens:   maxTokens,
	}

	if schema != nil {
		req.ResponseFormat = &ResponseFormat{
			Type: "json_object",
		}
	} else {
		req.ResponseFormat = &ResponseFormat{Type: "json_object"}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.CommandCodeAPIKey)

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("API call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	var gr response
	if err := json.Unmarshal(respBody, &gr); err != nil {
		return "", fmt.Errorf("parse response: %w\nRaw response: %s", err, string(respBody))
	}

	if len(gr.Choices) == 0 {
		return "", fmt.Errorf("no choices in response\nRaw response: %s", string(respBody))
	}

	content := strings.TrimSpace(gr.Choices[0].Message.Content)
	if content == "" {
		return "", fmt.Errorf("empty content in response\nRaw response: %s", string(respBody))
	}

	return content, nil
}

// CallStreamContext is Call with the text streamed as it is generated: onDelta is
// called with every content chunk, and the accumulated raw output is returned at the
// end so the caller can run exactly the same grounding verification as the
// non-streaming path. Streaming changes WHEN the reader sees the words, never whether
// they were checked.
//
// There is deliberately no mid-stream fallback to native Gemini: a stream that dies
// halfway cannot be resumed on another provider without the reader seeing two
// different summaries spliced together. On error the caller falls back to the plain
// non-streaming endpoint, which still has the Gemini fallback.
func (c *Client) CallStreamContext(ctx context.Context, prompt string, schema *Schema, onDelta func(string) error) (string, error) {
	if c.CommandCodeAPIKey == "" {
		return "", fmt.Errorf("COMMAND_CODE_API_KEY not configured")
	}

	req := request{
		Model: c.model(),
		Messages: []Message{
			{Role: "user", Content: prompt},
		},
		Temperature: 0.2,
		MaxTokens:   65536,
		Stream:      true,
	}
	if schema != nil {
		req.ResponseFormat = &ResponseFormat{Type: "json_object"}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.CommandCodeAPIKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("API call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(raw))
	}

	var out strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // keep-alive/comment frames and anything else non-JSON
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		if delta == "" {
			continue
		}
		out.WriteString(delta)
		if onDelta != nil {
			if err := onDelta(delta); err != nil {
				// The reader is gone (client disconnected): stop generating rather
				// than burning tokens into a closed connection.
				return out.String(), err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return out.String(), fmt.Errorf("stream read: %w", err)
	}
	if out.Len() == 0 {
		// Reasoning models sometimes stream only reasoning tokens and never
		// emit a content delta. Fall back to a non-streaming call (which
		// also has the native Gemini fallback) so the caller still gets a
		// usable answer instead of an empty stream.
		resp, _, err := c.Call(prompt, schema)
		if err != nil {
			return "", fmt.Errorf("empty stream from provider; fallback failed: %w", err)
		}
		if onDelta != nil && resp != "" {
			_ = onDelta(resp)
		}
		return resp, nil
	}
	return out.String(), nil
}
