# Guide: adding a real email provider

Status: **Resend is implemented** (`internal/notify/resend.go`, chosen 2026-10-06), selected with
`ORION_EMAIL_PROVIDER=resend`, `ORION_EMAIL_API_KEY` and `ORION_EMAIL_FROM`; startup fails if
either is missing. Steps 2 to 6 below are done. **Still to do by the owner:** step 1 (add the
sending subdomain in Resend and publish its SPF, DKIM and DMARC records), step 7 (a real delivery
test from staging to a Gmail and a local-ISP inbox) and step 8 (watch bounces and complaints in
the Resend dashboard). Locally, `ORION_EMAIL_PROVIDER=log` still writes mail to the log.

The rest of this guide is the original how-to, kept for adding or swapping a provider.

## What exists

* `internal/notify` defines `Sender` (`Send(ctx, Message{To, Subject, Text})`), `LogSender` and
  `MemorySender` (for tests). Callers depend only on the interface.
* Email is sent **only from river jobs** in `orion worker`, never in a request or a database
  transaction. If `Send` returns an error the job fails and river retries it with backoff, so a
  provider outage delays mail instead of losing it. Keep that: do not swallow errors in `Send`.
* Two places build a sender today, both hard-coded to `notify.LogSender` in `cmd/orion/main.go`
  (`worker()`): the identity jobs (verification email) and the device-health jobs (alerts). Future
  jobs (trial reminders, B5.x) will take the same `Sender`.
* Messages are plain text, in Indonesian or English per the recipient's locale
  (`internal/identity/emails.go`, `internal/sync/health.go`). Plain text is deliberate for now.
* `ORION_PUBLIC_URL` is the front-end base URL used to build links inside emails. It must be the
  real site in production.

## Choose a provider

You need transactional email (not marketing), a Jakarta-friendly delivery record to Indonesian
mailboxes (Gmail, Yahoo, Telkomsel and Indosat addresses are common), an API or SMTP, and a price
that is fine at a few thousand mails a month. Evaluate two or three, for example Amazon SES,
Resend, Postmark, Mailgun or SMTP2GO, and send a test to your own Gmail and a local-ISP address
before deciding. Record the decision as an ADR (`docs/adr/` in the product repository).

## Steps

1. **Domain.** Use a sending subdomain such as `mail.orion.example`, not the root domain. In the
   provider, add the domain and publish the DNS records it gives you: **SPF**, **DKIM** (usually
   two or three CNAMEs) and a **DMARC** record (`v=DMARC1; p=none; rua=mailto:...` at first,
   tightening to `quarantine` once reports are clean). Wait until the provider shows the domain as
   verified. Without this, verification links land in spam.
2. **Credentials** into the secret store (not the repo): an API key or SMTP username and password.
3. **Config** (`internal/config/config.go`): extend `EmailProvider` to accept the new name in the
   `switch` (today it only accepts `log`), and add the variables it needs, for example
   `ORION_EMAIL_FROM` (`Orion <no-reply@mail.orion.example>`), `ORION_EMAIL_API_KEY` or
   `ORION_SMTP_HOST/PORT/USER/PASSWORD`. Validate them at startup when the provider is selected, so
   a missing key fails the deploy and not the first signup. Add them to `.env.example` and the
   configuration section of `deploy/README.md`. Add a config test for the new validation.
4. **Implementation** (`internal/notify/<provider>.go`): a type that implements `Sender`.

   ```go
   type SMTPSender struct{ Addr, User, Pass, From string }

   func (s SMTPSender) Send(ctx context.Context, m Message) error {
       // Build the message: From, To, Subject, and
       //   Content-Type: text/plain; charset=UTF-8
       // Encode the subject with mime.QEncoding.Encode("utf-8", m.Subject) (Indonesian text).
       // Honour ctx (dial with a deadline); wrap errors with the provider's response.
       // Return nil only when the provider accepted the message.
   }
   ```

   For an HTTP API provider, post JSON with `http.NewRequestWithContext`, a client timeout
   (10 seconds), and treat any non-2xx as an error. Never log the message body or the API key; the
   body contains a one-time verification link, which is a credential.
5. **Wiring** (`cmd/orion/main.go`): replace the two `notify.LogSender{Logger: logger}` literals in
   `worker()` with one `sender, err := newSender(cfg, logger)` that returns the configured
   provider (`log` still allowed outside production). One function, one place to extend.
6. **Tests.** The real provider cannot run in CI, so test the parts that can break: build the
   message for a subject with non-ASCII characters and check the encoded headers; run the sender
   against a local fake (`httptest.Server` for an API, a tiny in-process SMTP listener for SMTP);
   check that a non-2xx response or a timeout returns an error. Keep the existing job tests on
   `MemorySender`.
7. **Try it for real** from staging: sign up, receive the verification mail, click the link; force
   an alert (`ORION_ALERT_UNSYNCED_AFTER=1m` with a paired device that reports old events) and check
   the owner and the operator copy (`ORION_ALERT_OPERATOR_EMAIL`) arrive.
8. **Bounces and complaints.** Turn on the provider's bounce and complaint notifications and watch
   them for the first weeks. A later task can add a webhook that marks an address undeliverable;
   until then, the operator reads the provider's dashboard.
9. **Docs.** Remove "no email provider yet" from `deploy/README.md`, update the open item in
   `CLAUDE.md`, and tell the frontend: verification and resend flows now work against real
   inboxes (`docs/API_CONTRACT.md`, section 3.1).

## Done when

A stranger's signup email arrives in a Gmail inbox, not spam, within a minute; the worker starts in
production with the provider configured and refuses to start without its key; and a provider
outage leaves jobs retrying, not failed or lost.
