// Package telegram is a minimal Telegram Bot API client: long polling for messages, sending and editing plain
// text. The bot token is part of every request URL, so no error from this package carries a URL.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"
)

// MaxText is Telegram's limit on a message's length, in characters.
const MaxText = 4096

// Client calls the Bot API for one bot.
type Client struct {
	base string // https://api.telegram.org/bot<token>
	http *http.Client
}

// New returns a client for the bot with this token. Endpoint is the API root; empty means Telegram's.
func New(token, endpoint string) *Client {
	if endpoint == "" {
		endpoint = "https://api.telegram.org"
	}
	return &Client{base: endpoint + "/bot" + token, http: &http.Client{Timeout: 90 * time.Second}}
}

// Update is one incoming event; only messages are used.
type Update struct {
	ID      int64    `json:"update_id"`
	Message *Message `json:"message,omitempty"`
}

// Message is a chat message.
type Message struct {
	ID   int64  `json:"message_id"`
	Chat Chat   `json:"chat"`
	From *User  `json:"from,omitempty"`
	Text string `json:"text,omitempty"`
	Date int64  `json:"date"`
}

// Chat is where a message was sent.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// User is a message's sender.
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username,omitempty"`
}

// APIError is an error the Bot API returned.
type APIError struct {
	Method      string
	Code        int
	Description string
	// RetryAfter is set when Telegram rate-limits the bot (code 429).
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// Updates long-polls for updates after offset (the last seen ID + 1), waiting up to wait for one to arrive.
func (c *Client) Updates(ctx context.Context, offset int64, wait time.Duration) ([]Update, error) {
	var out []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         int(wait.Seconds()),
		"allowed_updates": []string{"message"},
	}, &out)
	return out, err
}

// Send posts text to a chat, as a reply to replyTo when it is not zero, and returns the new message's ID.
func (c *Client) Send(ctx context.Context, chat int64, text string, replyTo int64) (int64, error) {
	params := map[string]any{
		"chat_id":              chat,
		"text":                 clip(text),
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
	if replyTo != 0 {
		params["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	var m Message
	if err := c.call(ctx, "sendMessage", params, &m); err != nil {
		return 0, err
	}
	return m.ID, nil
}

// Edit replaces the text of a message the bot sent.
func (c *Client) Edit(ctx context.Context, chat, message int64, text string) error {
	return c.call(ctx, "editMessageText", map[string]any{
		"chat_id":              chat,
		"message_id":           message,
		"text":                 clip(text),
		"link_preview_options": map[string]bool{"is_disabled": true},
	}, nil)
}

// Typing shows "typing…" in a chat for a few seconds.
func (c *Client) Typing(ctx context.Context, chat int64) error {
	return c.call(ctx, "sendChatAction", map[string]any{"chat_id": chat, "action": "typing"}, nil)
}

// Me returns the bot's own user, which checks the token.
func (c *Client) Me(ctx context.Context) (*User, error) {
	var u User
	if err := c.call(ctx, "getMe", map[string]any{}, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram %s: bad request", method)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, withoutURL(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("telegram %s: reading the response: %w", method, withoutURL(err))
	}
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d with an unreadable body", method, resp.StatusCode)
	}
	if !r.OK {
		return &APIError{Method: method, Code: r.ErrorCode, Description: r.Description,
			RetryAfter: time.Duration(r.Parameters.RetryAfter) * time.Second}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("telegram %s: unexpected result: %w", method, err)
	}
	return nil
}

// withoutURL drops the request URL (which holds the bot token) from a transport error.
func withoutURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// clip shortens text to Telegram's limit without splitting a character.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= MaxText {
		return s
	}
	r := []rune(s)
	return string(r[:MaxText-1]) + "…"
}
