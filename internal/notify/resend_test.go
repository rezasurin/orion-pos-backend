package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResendSenderPostsTheMessage(t *testing.T) {
	var got struct {
		From    string   `json:"from"`
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		Text    string   `json:"text"`
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"id":"e1"}`))
	}))
	defer srv.Close()

	s := ResendSender{APIKey: "re_test", From: "Orion <no-reply@mail.orion.test>", URL: srv.URL}
	m := Message{To: "budi@example.id", Subject: "Verifikasi email Anda — Orion", Text: "Klik tautan"}
	if err := s.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer re_test" || got.From != s.From || len(got.To) != 1 || got.To[0] != m.To ||
		got.Subject != m.Subject || got.Text != m.Text {
		t.Errorf("auth %q, body %+v", auth, got)
	}
}

func TestResendSenderFailsOnNon2xx(t *testing.T) {
	// A failed send must return an error so the river job retries instead of losing the mail.
	for _, code := range []int{http.StatusUnauthorized, http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		err := ResendSender{APIKey: "k", From: "f", URL: srv.URL}.Send(context.Background(), Message{To: "a@b.c"})
		srv.Close()
		if err == nil {
			t.Errorf("status %d: want an error", code)
		}
	}
}

func TestResendSenderHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (ResendSender{URL: srv.URL}).Send(ctx, Message{}); err == nil {
		t.Error("want an error for a cancelled context")
	}
}
