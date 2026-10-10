# Owner to-dos

Decisions and setup that only the owner can do. None of them blocks feature work; each one blocks
the task named next to it, or the pilot. Parked on 2026-10-06 to focus on features. Tick an item
off here when it is done, and update the open items in `CLAUDE.md`.

## Before the pilot

- [ ] **Choose hosting and deploy.** Follow `docs/guides/hosting-and-restore.md` (sections 1 to 3):
      a managed Postgres, the secrets in the platform's secret store (including `ORION_SECRETS_KEY`,
      backed up separately: losing it locks every operator out), TLS and a hostname, the deploy
      workflow on tag, and the first operator (`orion admin create-operator`).
- [ ] **Run the first restore drill** (same guide, section 5) and write it up in
      `docs/runbooks/restore.md`: date, who, which backup, how long it took, what went wrong.
- [ ] **Email through Resend** (`docs/guides/email-provider.md`, steps 1, 7 and 8):
  - [ ] DNS for the sending subdomain (for example `mail.orion.example`): SPF, DKIM, DMARC, then
        verify the domain in Resend. Set `ORION_EMAIL_PROVIDER=resend`, `ORION_EMAIL_API_KEY`,
        `ORION_EMAIL_FROM`.
  - [ ] A real delivery test from staging: sign up, receive the verification mail, click the link.
  - [ ] Turn on bounce and complaint notifications and watch them during the pilot.
- [ ] **Accountant's sign-off** on per-bill, half-up rounding of tax and service charge
      (`docs/BACKEND_PLAN.md` section 4.8). If they want another rule, it is a new pricing version.
- [ ] **Publish the terms of service** text for version `2026-10-06` (`identity.TermsVersion`); the
      front end shows it at signup and sends that version.

## Blocks B2.4 (QRIS through doit.id) and the gateway half of B2.5 (refunds)

- [ ] **Ask doit.id who pays the fee.** Their payment's `total_amount` is `amount + fee_amount`. The
      plan assumes the merchant absorbs it (the customer pays the bill amount). If the customer must
      pay it, the bill and the receipt change. See `docs/BACKEND_PLAN.md` section 6.5.1.
- [ ] **Get sandbox API keys** and the webhook secret, and confirm how sub-merchants (one per
      business) are onboarded with their licensed partner.

## Blocks B2.11 (security pass)

- [ ] **The front end's web addresses** (production and staging), for CORS. None are set now, so a
      browser on another origin cannot call the API.
- [ ] Hosting (above), for the header and secret-rotation parts.

## Decisions (defaults in force until you decide)

- [ ] **Turnstile on signup?** Default: no. Signup has a honeypot field and rate limits (5 per
      address per 2 minutes, 3 per email). Add Turnstile if bot signups appear.
- [ ] **Must existing users accept new terms?** Default: no. Users accept the version current at
      their signup; nothing asks again. When the terms first change, decide whether login should
      require accepting the new version.
- [ ] **Where the TypeScript API types are published** (B0.3, `docs/BACKEND_PLAN.md` section 3):
      the registry for the generated client package. Until then the front end works from
      `api/openapi.yaml` directly.
