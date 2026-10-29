package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const token = "123456:SECRET-token"

func TestSendAndUpdates(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bot"+token+"/") {
			t.Errorf("path %s lacks the token prefix", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		got = map[string]any{}
		_ = json.Unmarshal(body, &got)
		switch strings.TrimPrefix(r.URL.Path, "/bot"+token+"/") {
		case "sendMessage":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42,"chat":{"id":7,"type":"private"},"date":1}}`)
		case "getUpdates":
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":5,"message":{"message_id":3,"chat":{"id":7,"type":"private"},"text":"0xabc","date":1}}]}`)
		default:
			_, _ = io.WriteString(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
		}
	}))
	defer srv.Close()
	c := New(token, srv.URL)

	id, err := c.Send(context.Background(), 7, "hi", 3)
	if err != nil || id != 42 {
		t.Fatalf("Send = %d, %v", id, err)
	}
	if got["text"] != "hi" || got["chat_id"] != float64(7) || got["parse_mode"] != nil {
		t.Errorf("sendMessage params %v (want plain text, no parse_mode)", got)
	}

	ups, err := c.Updates(context.Background(), 5, 30*time.Second)
	if err != nil || len(ups) != 1 || ups[0].Message.Text != "0xabc" {
		t.Fatalf("Updates = %+v, %v", ups, err)
	}
	if got["offset"] != float64(5) || got["timeout"] != float64(30) {
		t.Errorf("getUpdates params %v", got)
	}
}

func TestErrorsNeverCarryTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 3","parameters":{"retry_after":3}}`)
	}))
	c := New(token, srv.URL)
	err := c.Edit(context.Background(), 1, 2, "x")
	if api, ok := errors.AsType[*APIError](err); !ok || api.RetryAfter != 3*time.Second {
		t.Errorf("rate limit not reported as APIError with RetryAfter: %v", err)
	}
	srv.Close()

	// A transport error: net/http puts the whole URL, token included, into *url.Error.
	_, err = c.Send(context.Background(), 1, "x", 0)
	if err == nil {
		t.Fatal("no error from a closed server")
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error leaks the URL or token: %v", err)
	}
}

func TestClip(t *testing.T) {
	long := strings.Repeat("é", MaxText+10)
	if n := utf8.RuneCountInString(clip(long)); n != MaxText {
		t.Errorf("clipped to %d characters, want %d", n, MaxText)
	}
	if clip("short") != "short" {
		t.Error("short text changed")
	}
}
