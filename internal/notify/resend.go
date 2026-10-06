package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ResendSender sends through Resend's HTTP API (https://resend.com/docs/api-reference/emails/send-email).
type ResendSender struct {
	APIKey string
	From   string // "Orion <no-reply@mail.example.id>", on a domain verified in Resend
	// URL overrides the API endpoint; tests point it at a fake.
	URL string
}

var resendClient = &http.Client{Timeout: 10 * time.Second}

func (s ResendSender) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(map[string]any{
		"from": s.From, "to": []string{m.To}, "subject": m.Subject, "text": m.Text,
	})
	if err != nil {
		return err
	}
	url := s.URL
	if url == "" {
		url = "https://api.resend.com/emails"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := resendClient.Do(req)
	if err != nil {
		return fmt.Errorf("resend: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		// The error body names the problem (bad key, unverified domain, rate limit); it never echoes
		// the message, whose text holds one-time links.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("resend: status %d: %s", resp.StatusCode, msg)
	}
	return nil
}
