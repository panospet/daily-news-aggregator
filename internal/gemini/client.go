package gemini

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Client communicates with the Google Gemini API.
type Client struct {
	HTTPClient *http.Client
	APIKey     string
}

// Caller is the interface for making Gemini API calls, enabling mock implementations in tests.
type Caller interface {
	Call(prompt string) (string, error)
	CallGrouping(prompt string) (string, error)
}

// ---------- request / response types ----------

type request struct {
	Contents []content      `json:"contents"`
	Config   generateConfig `json:"generationConfig"`
}

type content struct {
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text"`
}

type generateConfig struct {
	Temperature     float64 `json:"temperature"`
	MaxOutputTokens int     `json:"maxOutputTokens"`
	ResponseMIME    string  `json:"responseMimeType"`
}

type response struct {
	Candidates []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
}

// ---------- public methods ----------

// Call sends a prompt using gemini-2.5-flash-lite with 8K max tokens.
func (c *Client) Call(prompt string) (string, error) {
	return c.CallWithModel(prompt, "gemini-2.5-flash-lite", 8192)
}

// CallGrouping sends a prompt using gemini-2.5-flash with 64K max tokens.
func (c *Client) CallGrouping(prompt string) (string, error) {
	return c.CallWithModel(prompt, "gemini-2.5-flash", 65536)
}

// CallWithModel sends a prompt to a specific Gemini model.
func (c *Client) CallWithModel(prompt, model string, maxTokens int) (string, error) {
	if c.APIKey == "" {
		return "", fmt.Errorf("GEMINI_API_KEY not configured")
	}

	req := request{
		Contents: []content{
			{Parts: []part{{Text: prompt}}},
		},
		Config: generateConfig{
			Temperature:     0.2,
			MaxOutputTokens: maxTokens,
			ResponseMIME:    "application/json",
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, c.APIKey)

	resp, err := c.HTTPClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("gemini API call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("gemini API error %d: %s", resp.StatusCode, string(respBody))
	}

	var gr response
	if err := json.Unmarshal(respBody, &gr); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("no candidates in Gemini response")
	}

	return strings.TrimSpace(gr.Candidates[0].Content.Parts[0].Text), nil
}
