package explain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client calls any OpenAI-compatible chat endpoint: Ollama on the same machine, or a free tier such as Groq, OpenRouter
// or Gemini. rugcheck has no model of its own and needs no paid key.
type Client struct {
	URL, Key, Model string
	HTTP            *http.Client
}

// FromEnv builds a client from RUGCHECK_LLM_URL, RUGCHECK_LLM_MODEL and (optional) RUGCHECK_LLM_KEY; nil when no model
// is configured, which is a normal way to run: the verdict goes out without the explanation.
func FromEnv() *Client {
	url, model := strings.TrimSpace(os.Getenv("RUGCHECK_LLM_URL")), strings.TrimSpace(os.Getenv("RUGCHECK_LLM_MODEL"))
	if url == "" || model == "" {
		return nil
	}
	return &Client{
		URL: strings.TrimRight(url, "/"), Model: model, Key: strings.TrimSpace(os.Getenv("RUGCHECK_LLM_KEY")),
		HTTP: &http.Client{Timeout: 5 * time.Minute},
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Complete asks the model once. Errors say what happened without the endpoint or the key: both can be secrets, and the
// bot logs errors.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.Model, "temperature": 0, "max_tokens": 300,
		"messages": []chatMessage{{"system", system}, {"user", user}},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", errors.New("llm: bad request URL")
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("llm: %w", ctx.Err())
		}
		return "", errors.New("llm: the request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", errors.New("llm: reading the answer failed")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", errors.New("llm: the answer is not JSON")
	}
	if len(out.Choices) == 0 {
		return "", errors.New("llm: the answer has no choices")
	}
	return out.Choices[0].Message.Content, nil
}
