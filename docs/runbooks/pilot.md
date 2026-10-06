# Pilot runbook

For whoever is on call during the design partner's weeks. Four jobs: read a review flag, fix a
stuck device, rebuild a report, and check the server is still fast. Everything here is built; the
design behind it is in `docs/BACKEND_PLAN.md` (sections 4.11.1, 5.1.1, 6.4.1 to 6.4.3).

The one rule: **a sale that reached the server is never dropped.** The server stores a sale as the
device rang it up and raises a flag when something looks wrong. A flag asks a person to look; it
never changes the money.

## 1. Reading a review flag

Flags show up on `GET /v1/sales?flagged=true` (the `flag_codes` list on each row) and in full on
`GET /v1/sales/{saleId}` (`flags[]` with a `detail` object). Voids, shifts and cash movements carry
flags too; `target_type` says which.

| Code | What it means | What to do |
|---|---|---|
| `total_mismatch` | The amounts on the sale do not recompute from its lines with pricing v1. `detail` has both sets of numbers. | Compare with the paper receipt. Almost always an app bug or a rounding rule the accountant has not agreed to (see below). Report it to the app team with the receipt number. The money stands as rung up. |
| `pricing_invalid` | The lines and discounts are not a bill the algorithm can price (for example a discount larger than the line). | Look at `detail`. Treat as an app bug; ask the cashier what they did. |
| `pricing_version` | The device priced with an algorithm version the server does not know. | The tablet runs a newer app than the server. Check the app version on the device (`support-report`). Not urgent for the sale itself. |
| `payment_mismatch` | Payments do not add up to what was due. `detail` has `due` and `paid`. | Check the drawer count for that shift. Change given in cash is normal and does not flag; anything else is a real difference. |
| `permission_missing` | The person did the action without the permission (`detail.permission`, `detail.staff_id`). | A manager decides. The action stands. If the role is wrong, fix the role; if the person overstepped, talk to them. |
| `stale_price` | A line used a price from before an update the tablet could have had. `detail.lines` lists them. | Usually a price changed mid-shift on a tablet that was offline. Check the price is right now; no money action. |
| `sale_outside_shift` | Rung up before the shift opened or after it closed. | The shift open or close event is probably late or missing. Check the shift on the end-of-shift report. |
| `shift_other_device` | On a shift that another device opened. | Fine if the cafe shares a shift between tablets; otherwise ask who used which tablet. |
| `device_time_ahead` | The device's clock was more than 10 minutes ahead of the server when the event arrived. | The tablet's clock is wrong. Fix the clock; the business date may be affected (see below). |
| `after_shift_close` | A cash movement after the shift closed. | It is not in that shift's expected cash. Check with the manager. |

Events the server **rejected** (not flagged) are a different thing: they were not stored. The
device sees the code in the push result. The usual codes are `malformed`, `invalid_payload`,
`invalid_receipt_number`, `idempotency_conflict`, `id_conflict`, `device_revoked`, and `abandoned`
(a parked event someone gave up on, section 2). Each is listed per tenant by `support-report`.

**Business date.** A sale's `business_date` is its `device_time` in the outlet's time zone minus
the day cutoff. A wrong tablet clock therefore puts sales on the wrong day. The sale is still there;
the fix is the clock, not the data.

**Rounding.** The pilot uses half-up rounding per bill. Get the accountant to confirm this before
the first week; a mismatch between their expectation and `total_mismatch` flags is a rounding
question before it is a bug.

## 2. Fixing a stuck device

Symptoms: an owner gets an email ("events not delivered" or "device silent during an open shift"),
the cashier says the sync dot is red, or end-of-day totals are short.

Thresholds: an alert opens when the oldest undelivered event is older than 30 minutes
(`ORION_ALERT_UNSYNCED_AFTER`), or a device with an open shift has been silent for 3 hours
(`ORION_ALERT_SILENT_AFTER`). Alerts close by themselves once the device catches up. Each opens one
email to the owners (and to `ORION_ALERT_OPERATOR_EMAIL` if set), not one every five minutes.

A daily check catches the tablets no alert covers (closed shift, nothing pending, just quiet):

```
orion admin stopped-syncing --operator you@orion.example            # silent for 24 hours
orion admin stopped-syncing --operator you@orion.example --quiet 4h
```

It lists, across every business that is not suspended, the tablets that synced (or were paired)
in the last 30 days but not within `--quiet`, the longest silent first. "Seen since" means the app
is running and talking to the server but its sync is failing: go to step 1. A business's daily
usage (sales, voids, events, rejected events, tablets that synced, flags) is
`orion admin metrics --operator you@orion.example --tenant SLUG [--days 14]`, computed nightly by
`orion worker`. The same views, and the changes below, are on the `/admin` API for a console
(`docs/API_CONTRACT.md` section 12).

A lost or stolen tablet the owner cannot reach is revoked with
`orion admin revoke-device --operator you@orion.example --device ID --reason "..."` (the id is in
`support-report`); the business sees it in its own audit log. A design partner moves to unlimited
with `orion admin set-plan --operator you@orion.example --tenant SLUG --plan early_access --reason "..."`.

Step 1: see what the server sees.

```
orion admin support-report --operator you@orion.example --tenant SLUG --reason "pilot: stuck device"
```

It prints, per device: last contact, clock skew, app version, events the device says it still holds,
open alerts; then events **parked** waiting for a record, **rejected** events and flag counts of the
last week by code, and the latest flagged sales. It is read-only but audited (`support.report_viewed`).

Step 2: decide what it is.

* **No recent contact, undelivered events** → the tablet is offline or the app is stuck. Get the
  cafe on a working connection; the outbox sends itself and the alert closes. Nothing is lost
  while it waits on the tablet. Do not wipe the app or log the device out before the outbox is
  empty: the outbox lives on the tablet.
* **Contact is recent but events keep coming back `retry`** → a server fault. Check the server logs
  for "sync:" errors and the worker is running. The device will resend on its own.
* **Events parked** (`pending_dependency`) → the event refers to a record the server has never
  received (a void for a sale it never got, a sale on an unknown shift). They release on their own
  the moment the missing record arrives. If the tablet is healthy and the record will never come
  (the tablet was reset, a draft was lost), give up on it:

  ```
  orion admin abandon-event --operator you@orion.example --tenant SLUG --event EVENT_ID --reason "why"
  ```

  The event becomes rejected with the code `abandoned` and is audited (`sync.event_abandoned`).
  Only parked events can be abandoned; accepted ones cannot, by design. Write the reason as if the
  accountant will read it: they will.
* **Rejected `device_revoked`** → someone revoked the device. Re-enroll it; events from before the
  revocation that were already accepted stay.
* **Clock skew large** → fix the tablet clock; see `device_time_ahead` above.

Step 3: confirm. After the device syncs, `support-report` should show no open alert and nothing
parked for it, and the end-of-day report (section 3) for the day should match the cafe's own totals.

## 3. Rebuilding a report

There is nothing to rebuild. Reports are computed live from the sales, voids, cash movements and
shifts every time they are read, so a late event or an abandoned one is reflected the next time the
report is fetched, and there is no stale copy to repair.

* `GET /v1/reports/shifts/{shiftId}`: end of shift. Expected cash = opening + cash received −
  refunds paid in this shift + pay-ins − pay-outs; shown next to counted cash, with the difference,
  and sales by payment method, voids and discounts.
* `GET /v1/reports/days/{date}?outlet_id=...`: end of day for one outlet, by business date.

If a number is wrong, the cause is a **missing or late event** (the device has not synced; section
2) or a **flag** (section 1), never a cached total. To compare with the device: the device's
receipt counter for the day should equal the number of `sale` rows plus unsent drafts; gaps are
explained by voids and by events still in the outbox. That comparison is the Phase 1 exit check.

## 4. Running the load check

```
make load
```

Needs Docker, or `ORION_TEST_DATABASE_URL` pointing at a Postgres you can create databases on.
It pushes a simulated week of a busy cafe (600 sales a day, 3 devices) in bursts and prints the
events per second and p95 push latency. The bar is **p95 under 300 ms**; the last run was about
116 ms at roughly 620 events/s with nothing flagged. Run it again after any change to the sync
core, the sales projectors or the report queries. It also checks that the report queries use their
indexes.

The full suite is `go test -count=1 -p 1 ./...`. `-p 1` matters: the packages share one template
database.
