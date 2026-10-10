# Orion API contract: the frontend guide

For the people and AI agents building the **back office** (web) and the **POS** (the offline-first
cashier tablet app). It says how to talk to the backend, what each screen calls, and the rules that
are easy to get wrong. It describes what is **built today** (Phase 0 and Phase 1).

**Sources of truth, in order:**

1. `api/openapi.yaml`: every endpoint, request, response and error. Generate your client and types
   from it; never hand-write them. If this guide and the spec disagree, the spec wins; tell us.
2. `testdata/pricing-vectors/`: 51 golden bills. The POS must reproduce every one (section 7).
3. This guide: the behaviour the spec cannot express (sync, offline rules, reports).
4. `docs/BACKEND_PLAN.md`: why it is built this way.

When the backend contract changes, `api/openapi.yaml` changes in the same commit and this guide is
updated if behaviour changed. Check `git log -- api/openapi.yaml docs/API_CONTRACT.md` for what is
new since you last looked.

---

## 1. Conventions

| Topic | Rule |
|---|---|
| Base URL | `/` on the server (`:8080` locally). Every business endpoint is under `/v1`. Operator endpoints are under `/admin` and are not for the apps. |
| Format | JSON, `snake_case` field names. Send `Content-Type: application/json`. Request bodies are at most 1 MB. |
| Money | **Integer rupiah**, always. Never a float, never a string, never cents. Rates are **basis points** (`1100` = 11%). |
| Time | RFC 3339 in UTC (`2026-10-04T08:30:00Z`). Dates (`business_date`, report `date`) are `YYYY-MM-DD`. Convert to the outlet's time zone only for display. |
| Ids | UUIDs. The POS creates its own ids for events as **UUIDv7** (section 6). |
| Paging | `?limit=` (1 to 200, default 50) and `?cursor=` set from the previous response's `next_cursor`. No `next_cursor` means the last page. Cursors are opaque: do not build or parse them. Some are UUIDs today; do not rely on it. |
| Absent vs null | Optional fields are omitted or `null`; treat both as "not set". In a `PATCH`, a field you leave out is unchanged. |
| Request id | Error bodies carry `request_id`; quote it when reporting a problem. |
| Health | `GET /healthz` (alive), `GET /readyz` (database reachable). |
| CORS | The server sets **no CORS headers yet**. A browser app on another origin needs a reverse proxy on the same origin, or a backend change. Ask before assuming. |

### Errors

Every error is `application/problem+json` (RFC 9457):

```json
{ "type": "about:blank", "title": "Forbidden", "status": 403, "code": "forbidden",
  "detail": "you do not have permission: report.view", "request_id": "..." }
```

**Switch on `code`, never on `title` or `detail`.** `detail` is for logs and may change or be in
either language. Show your own translated message per `code`.

| Status | `code` | Meaning and what to do |
|---|---|---|
| 400 | `validation_failed` | The request is malformed or breaks a rule. `detail` names the field. Fix the input; do not retry as is. |
| 401 | `invalid_credentials` | Wrong email/password (or operator code). |
| 401 | `invalid_token` | The access token is missing, wrong, expired or already used. Refresh once, then sign in again. |
| 401 | `token_reused` | A refresh token was used twice, so the whole session was revoked. Sign in again. |
| 403 | `forbidden` | Signed in but lacks a permission or outlet. `detail` names the permission. Hide the control instead where you can. |
| 403 | `email_not_verified` | Verify the email first (section 3.1). |
| 403 | `no_tenant` | The account belongs to no business. |
| 403 | `tenant_suspended` | The business is suspended and this is a change. The back office stays readable: show a "suspended, read-only" banner (also when `me.tenant.suspended_at` is set) and disable editing. |
| 403 | `device_revoked` | This tablet was revoked. Stop syncing, keep the outbox, show "ask the owner to pair this device again". |
| 403 | `module_disabled` | The plan does not include this module. Hide it (see entitlements). |
| 403 | `limit_reached` | A plan limit (devices, staff, outlets) is full. Say which; `detail` has the number. |
| 404 | `not_found` | No such thing **in this business**. A resource of another business is also a 404, never a 403. |
| 409 | `conflict` | The change clashes with current state (duplicate, already done). `detail` explains. |
| 409 | `terms_outdated` | At signup: the accepted terms are not the current version. Reload the terms and ask again. |
| 409 | `tenant_required` | At login: the account belongs to several businesses. `tenant_ids` lists them; ask which and login again with `tenant_id`. |
| 413 | `payload_too_large` | Body over 1 MB. For a push, send fewer events; for a catalog import, split the file. |
| 429 | `rate_limited` | Wait `Retry-After` seconds (header) and retry. Signup, login, token exchange, pairing and email endpoints are limited. |
| 500 | `internal` | Our fault. Retry with backoff; quote `request_id` if it persists. |
| 503 | `admin_disabled` | Operator console off. Not for the apps. |

---

## 2. Who calls what: three kinds of token

Every request except the public ones carries `Authorization: Bearer <access_token>`. There are
three kinds of token and an endpoint accepts only its own kind (using the wrong one is `401`).

| Caller | Gets a token from | Lifetime | Used for |
|---|---|---|---|
| **User** (owner or manager, on the back office, or when pairing a tablet) | `POST /v1/auth/login` | access 15 min, refresh 30 days, single use | everything marked "user" below |
| **Device** (a paired POS tablet) | `POST /v1/devices/token` with the stored device secret | 30 min | `/v1/sync/*`, `/v1/pos/*` |
| **Operator** (Orion staff) | `/admin/auth/*` | short | `/admin/*`, not for the apps |

Cashiers and baristas **never sign in with email**. They tap a PIN on the tablet, checked locally
against the roster (section 5). The tablet itself authenticates as a device.

Whether a user may call an endpoint is checked by the server from their role. `GET /v1/me` returns
the permissions to drive the UI; the server still enforces them, so the UI is only convenience.

### Permissions and the default roles

`sale.create`, `sale.void`, `sale.refund`, `discount.apply_manual`, `drawer.open_no_sale`,
`shift.open`, `shift.close`, `catalog.manage`, `inventory.manage`, `report.view`, `staff.manage`,
`device.manage`, `settings.manage`, `kitchen.view`.

| Role | Holds |
|---|---|
| Owner | all of them, at every outlet |
| Manager | all except `settings.manage` |
| Cashier | `sale.create`, `shift.open`, `shift.close` |

Roles are assigned **per outlet** (`outlet_roles`), so a person can be manager at one outlet and
cashier at another. `permissions` in `/v1/me` is the union over outlets; the roster gives the
list for one outlet.

---

## 3. Back office (user token)

### 3.1 Sign in and session

| Call | Notes |
|---|---|
| `POST /v1/auth/signup` `{business_name, owner_name, email, password, terms_version, outlet_name?, outlet_code?, locale?, website?}` | Public. Creates the business, its first outlet (`outlet_code` defaults to `OUT1`, `outlet_name` to the business name), the default roles and the owner, then emails a verification link. **Always an empty `202`** for a well-formed request, including when the email already has an account (nothing is created then), so after signup show "check your inbox" either way and offer resend. `terms_version` is the version of the terms you showed and the user ticked; anything but the current one is `409 terms_outdated` (`detail` names the current one): reload the terms and ask again. `website` is a honeypot: render it hidden from people (off-screen, `tabindex="-1"`, `autocomplete="off"`) and always send it empty. Limited to 5 per address (refill 1 per 2 minutes) and 3 per email. |
| `POST /v1/auth/login` `{email, password, tenant_id?}` | Public. Returns a `Session` (`access_token`, `refresh_token`, expiry times, `tenant_id`, `user_id`). `409 tenant_required` when `tenant_id` is needed. `403 email_not_verified` before verification. |
| `POST /v1/auth/refresh` `{refresh_token}` | Public. Returns a new `Session`. **Every refresh token works once**: store the new pair before using it, and serialise refreshes (two tabs refreshing at once will trip `token_reused` and sign the user out). Refresh when the access token is about to expire or on the first `401 invalid_token`. |
| `POST /v1/auth/logout` `{refresh_token}` | Public, idempotent (always `204`). |
| `POST /v1/auth/verify-email` `{token}` | Public. The token comes in the email link. |
| `POST /v1/auth/resend-verification` `{email}` | Public. Always `202`, whether or not the address exists. |
| `GET /v1/me` | The user, the business (`subscription_status`, and `suspended_at` while suspended: show a read-only banner), `is_owner`, `permissions[]`. Call after login to build the menu. |
| `GET /v1/entitlements` | `items[]` of `{key, kind, category, value, source}`: modules on/off, limits (`-1` = unlimited), flags. Hide a module when its key is `0`. A self-serve signup is on the **free plan**: `limit.outlets` 1, `limit.devices` 2 (paired, not revoked), `limit.staff` 5 (active staff, **the owner counts**), both modules off; its `subscription_status` is `active`. Businesses an operator creates are on early access (`early_access`, everything unlimited). Show "x of y used" from these values and expect `403 limit_reached` when full. |

Staging and production send real email (Resend). Local development writes each email to the
worker's log, so take the verification link from there. The seeded demo business
(`./bin/orion admin seed-demo`) is already verified: `owner@demo.orion.test` / `demo-password-1`.

### 3.2 Business, outlets and settings

| Call | Permission | Notes |
|---|---|---|
| `GET /v1/outlets` | any user | Active outlets with settings, ordered by code. |
| `GET /v1/outlets/{outletId}` | any user | One outlet. |
| `PATCH /v1/outlets/{outletId}/settings` | `settings.manage` | Partial update. Sales already recorded keep their old amounts and business date. Devices get the change at their next pull. |
| `GET /v1/announcements` | any user | Notices from Orion (maintenance, billing) in force now, to every business or to this one, newest first, in the user's locale. Show `warning` as a banner and `info` quietly; fetch after login and every few minutes. Works while suspended. |

Outlet settings (all affect how a bill is calculated, see section 7):

| Field | Values |
|---|---|
| `timezone` | `Asia/Jakarta`, `Asia/Makassar`, `Asia/Jayapura` |
| `business_day_cutoff` | `"HH:MM"`, `00:00` to `11:59`. A sale before it belongs to the previous business day. |
| `price_includes_tax` | bool |
| `tax_rate_bp`, `service_charge_rate_bp` | 0 to 10000 |
| `service_charge_taxable` | bool |
| `cash_rounding_unit` | rupiah, `0` = none (typically `100` or `500`) |
| `cash_rounding_mode` | `nearest`, `down`, `up` |
| `receipt_header`, `receipt_footer` | text, at most 500 characters |

An outlet's `code` (2 to 6 uppercase letters or digits) is the prefix of its receipt numbers and is
not editable here.

### 3.3 Staff, roles and PINs (`staff.manage`)

| Call | Notes |
|---|---|
| `GET /v1/roles` | Roles with their permissions, to fill the role picker. |
| `GET /v1/staff` | Paged. |
| `POST /v1/staff` `{display_name, outlet_roles[], pin?}` | `limit_reached` when the plan's staff limit is full. |
| `PATCH /v1/staff/{staffId}` | `display_name`, `active`, `outlet_roles` (replaces the list). Deactivate instead of deleting. |
| `PUT /v1/staff/{staffId}/pin` `{pin}` | 4 to 6 digits. The PIN is never readable again; `has_pin` and `pin_rotated_at` tell its state. |

A change reaches tablets at their next pull. A deactivated person holds no permissions, so a new event under their name is accepted but flagged
`permission_missing`; events they made earlier stay valid.

### 3.4 Devices (`device.manage`)

| Call | Notes |
|---|---|
| `GET /v1/devices` | Paged. Each device has health fields (section 9). |
| `POST /v1/devices/pair` `{outlet_id, name}` | Done on the tablet while an owner or manager is signed in. Returns `{device, device_secret}`. **The secret is shown once**: store it in the device's secure storage; the server keeps only a hash. Losing it means pairing again. `device.device_code` becomes part of receipt numbers and is never reused. `403 limit_reached` when the plan's device limit is full; revoking a device frees its slot. |
| `DELETE /v1/devices/{deviceId}` | Revoke. Idempotent. The tablet's next token exchange fails with `device_revoked`. |

### 3.5 Catalog (`catalog.manage`)

What the business sells. Archived entries stay (old sales refer to them): list with
`include_archived=true` to see them, and archive or restore with `"archived": true|false`.

| Call | Notes |
|---|---|
| `GET/POST /v1/categories`, `PATCH /v1/categories/{categoryId}` | `{name, sort_order}`. |
| `GET/POST /v1/items`, `GET/PATCH /v1/items/{itemId}` | An item has at least one **variant** (a size or option with its own price). A one-size item has one variant with an empty `name`. An item lists its `modifier_group_ids` in display order; `PATCH` replaces that list. `clear_category: true` removes the category. `station_id` routes it to a kitchen station; `clear_station: true` removes it (no ticket). `sku`/`barcode`/`image_url`: a blank string clears. A null `category_id` or `station_id` is left out of the response. |
| `GET/POST /v1/kitchen-stations`, `PATCH /v1/kitchen-stations/{stationId}` | Where items are made, for kitchen and bar tickets: `{name (max 40), sort_order}`. Live names are unique per business, ignoring case. Archive rather than delete. |
| `POST /v1/items/{itemId}/variants`, `PATCH /v1/variants/{variantId}` | `base_price` in rupiah, 0 to 1,000,000,000. |
| `GET/POST /v1/modifier-groups`, `PATCH /v1/modifier-groups/{groupId}` | A group (for example "Sugar") has `min_select`, `max_select` and `required` (a required group needs `min_select` of at least 1). |
| `POST /v1/modifier-groups/{groupId}/modifiers`, `PATCH /v1/modifiers/{modifierId}` | `price_delta` in rupiah, **may be negative**. |
| `GET /v1/outlets/{outletId}/variants` | Per-outlet price and availability. |
| `PUT /v1/outlets/{outletId}/variants/{variantId}` `{price_override?, available}` | `price_override: null` means the base price. A variant with no row is at base price and available. |

The price a sale line uses is `price_override` if set, else `base_price`, plus the chosen
modifiers' `price_delta`. The POS does this from its pulled copy, offline.

#### Kitchen and bar tickets (printed by the POS)

The server only says **which station makes each item** (`item.station_id`); the tablet prints.
When a sale is completed, group its lines by their item's station and print one ticket per
station: receipt number, time, cashier, then each line's name, quantity and modifiers. Lines
whose item has no station, or whose station is archived, get no ticket. Which printer a station
uses is a setting on the tablet (printers live on the outlet's network), so offer a "station →
printer" mapping in the POS settings, from `kitchen_stations` in the pull. Nothing is sent back:
there is no ticket status until the restaurant flow (Phase 4) adds a kitchen display.

#### Importing a menu from CSV

`POST /v1/catalog/import?dry_run=true|false`. The body is the file itself: `Content-Type: text/csv`,
UTF-8, at most 2000 rows and 1 MB. Comma or semicolon separated (Excel in an Indonesian locale
writes semicolons); a byte order mark is fine. Columns are found by header, in any order, ignoring
case, and other columns are ignored, so **Moka's item export imports as is**:

| Field | Headers read | Notes |
|---|---|---|
| item name (required) | `item_name`, `Items Name`, `nama item`, `nama produk` | Rows with the same name (any case) are one item, one variant per row. |
| price (required) | `price`, `Basic - Price`, `harga` | Whole rupiah: `25000`, `25.000`, `25,000`, `Rp 25.000`, `25000.00`. Cents are refused. |
| variant name | `variant_name`, `Variant Name`, `nama varian` | Empty for a one-size item. Required to tell apart two rows of one item. |
| category | `category`, `kategori` | Matched by name to an existing category, else created. One item, one category. |
| sku, barcode | `sku`, `barcode` | Go on the variant; must not be in use already. |
| track stock | `track_stock`, `Track Stock` | `yes`/`no` (`ya`/`tidak`, `true`/`false`, `1`/`0`). |

Offer a template with the header `category,item_name,variant_name,price,sku,barcode,track_stock`.

- **The import only adds.** An item whose name is already in the catalog is a row error, so
  sending the same file twice adds nothing. Modifiers and images are not imported; add them in
  the catalog manager afterwards.
- **Flow:** send with `dry_run=true`, show `categories`, `items`, `variants` (what would be created)
  and `errors`; when the owner confirms, send the same file with `dry_run=false`.
- **All or nothing:** if any row has an error, nothing is written and `committed` is `false`.
- Each error has `row` (the line in the file, header = 1), `column` (the field above) and a
  `message` in English to show next to the row.
- A problem with the whole file (empty, not CSV, not UTF-8, missing a required column, too many
  rows) is `400 validation_failed`, with `detail` saying which. Over 1 MB is `413`.
- Tablets get the new items in their next pull.

### 3.6 Reports and sales (`report.view`)

All read-only and **computed live**: there is nothing to refresh or rebuild, and a late-arriving
sale shows up the next time you fetch.

| Call | Notes |
|---|---|
| `GET /v1/reports/shifts/{shiftId}` | End of shift: `cash` (opening, received, refunded, pay_in, pay_out, `expected`, and once closed `counted` and `difference`), `sales` totals, `payment_methods[]`, `discounts`, `voided_sales`, `voids_recorded`, `refunds_recorded` (refunds made in this shift), `no_sale_openings`, `flags`. |
| `GET /v1/reports/days/{date}?outlet_id=` | End of day for one outlet by **business date**: the same sections, plus `refunds` (made that day, of any day's sales), `cash_movements`, `shifts[]` with each one's reconciliation, `open_shifts`, and `flags` by code. |
| `GET /v1/reports/sales?outlet_id=&from=&to=&group_by=` | Completed sales of one outlet over business dates `from`..`to` (inclusive, at most 366 days). `group_by=day`: `days[]`, one per date **including days without sales** (zeros), each with the end-of-day `sales` totals. `group_by=item`: `items[]`, one per variant sold, highest `net` first, with `item_name`/`variant_name` as in the catalog **now** (a renamed item shows its new name), `quantity`, `gross`, `discounts` (its own and its share of bill discounts) and `net`; the `net` column adds up to the range's net sales; `refunded_quantity` and `refunded` are refunds made in the range (a variant only refunded, not sold, in the range comes last with zero sales). Each day also has `refunds {count, total}`, and each payment method `refunds` and `refunded`. `group_by=payment_method`: `payment_methods[]`. Only the list for `group_by` is present. `format=csv` returns the same rows as `text/csv` (header row, comma separated, whole rupiah) for a "Download" button: fetch it with the token and save the blob. In the CSV a name starting with `=`, `+`, `-` or `@` is prefixed with `'` so a spreadsheet does not run it as a formula. |
| `GET /v1/sales` | Newest first. Filters: `outlet_id`, `from`, `to` (business dates, inclusive), `status` (`completed`, `voided`), `receipt_number` (exact), `staff_id`, `flagged`, plus `cursor`, `limit`. Without `outlet_id` it covers every outlet the caller can see. |
| `GET /v1/sales/{saleId}` | Everything: lines with modifiers, discounts, payments, the void, `refunds[]` (each with its `lines[]` by `line_no`), flags with their `detail` (`target_type` sale, void or refund), and the settings the device priced with. Every sale in the list and detail has `refunded`, the total given back so far. |

How to read the numbers:

* **Expected cash** = opening cash + cash received − cash given back in this shift (for voids made
  in it, and cash refunds made in it) + pay-ins − pay-outs. `difference` = `counted − expected`; **negative means the drawer is short**.
  Both are absent while the shift is open.
* Cash `received` is net of change given. A cash sale of 47,000 paid with a 50,000 note counts 47,000.
* `sales.total` is the bill **before cash rounding**; `sales.rounding` is the cash rounding (positive
  or negative). What actually went into the drawer for cash is `total + rounding`.
* A sale belongs to the business date it was **rung up** on; a void made later removes it from
  that day's totals but the void itself is counted in the shift it happened in (`voids_recorded`).
* A **refund** leaves its sale as rung up, in its own day; it counts on the day and in the shift it
  was made (`refunds`, `refunds_recorded`, `payment_methods[].refunded`). Net takings for a day are
  the sales totals minus the day's refunds.
* Amounts are as on the receipt: a sale is stored exactly as the device charged it.

**Review flags** (`flag_codes` on a sale, `flags` on reports). A flag asks a person to look; it
never changes the money or blocks a sale. Show them as a badge and link to the sale detail.

| Code | Plain meaning (for tooltips and the owner) |
|---|---|
| `total_mismatch` | The amounts do not recompute from the lines. |
| `pricing_invalid` | The lines and discounts are not a bill the algorithm can price. |
| `pricing_version` | Priced by an app version the server does not know. |
| `payment_mismatch` | Payments do not add up to what was due. |
| `permission_missing` | The person lacked the permission for this action. |
| `stale_price` | A price from before an update the tablet could have had. |
| `sale_outside_shift` | Rung up before the shift opened or after it closed. |
| `shift_other_device` | On a shift another tablet opened. |
| `device_time_ahead` | The tablet's clock was more than 10 minutes ahead. |
| `after_shift_close` | A cash movement after the shift closed. |
| `refund_over_quantity` | Over all its refunds, more of a line was given back than was sold. |
| `refund_over_paid` | Over all its refunds, more money was given back than the sale took. |

The runbook `docs/runbooks/pilot.md` tells the operator what to do about each.

---

### 3.7 Inventory setup (`inventory.manage`, inventory module)

The Setup pages of the back office. The rules are the back office's `docs/BUSINESS_RULES.md`; ids
such as BR-UOM-06 refer to it. Every call here needs `inventory.manage` **and** the inventory module
in the plan: without it the answer is `403 module_disabled` (the free plan does not include it; check
`module.inventory` in `GET /v1/entitlements` and hide the menu). Nothing here reaches the POS.

Common to all four: entries are archived, never deleted (`include_archived=true` lists them,
`"archived": true|false` in a `PATCH`), and an archived entry cannot be picked for something new
(BR-GEN-06), though what already points at it keeps it. **Names** are trimmed, 2 to 100 characters
(units and packs 2 to 60), unique per business ignoring case among live entries (`409`), so archiving
frees a name. A blank `description` clears it.

| Call | Notes |
|---|---|
| `GET/POST /v1/expense-types`, `PATCH .../{expenseTypeId}` | What money spent is booked as: `{name, group, description?}`. `group` is `cost_of_goods` (ingredients and supporting materials), `operating` (rent, utilities, salaries, supplies), `maintenance`, `marketing_event` or `capital` (BR-EXP-02). It is a classification only: "paid", "on credit" or "debt payment" belong to the payment, not the type. |
| `GET/POST /v1/stock-categories`, `PATCH .../{stockCategoryId}` | Groups stock items (BR-CAT): `{name, default_expense_type_id?, description?}`. The default pre-fills a purchase line; `clear_default_expense_type: true` removes it. |
| `GET/POST /v1/uom-categories`, `PATCH .../{uomCategoryId}` | A unit category with all its `units`, the reference first, then from small to large. See below. |
| `GET/POST /v1/stock-items`, `PATCH .../{stockItemId}` | Anything bought, stored, prepared or used (BR-ITM); menu items stay in the catalog. See below. `?category_id=` and `?type=` filter the list. |

**Units** (ADR 0009, BR-UOM):

- A unit category is a dimension: weight, volume or count. Every business starts with the
  **standard** ones (`is_standard: true`): *Berat* with `g` (reference) and `kg`, *Volume* with `ml`
  (reference) and `L`, *Jumlah* with `pcs` (reference), `lusin` (12) and `kodi` (20). A standard unit
  keeps its symbol and ratio and stays active; a standard category is never archived. Both can be
  renamed, and owners add their own units to them (for example `ons` = `100/1` g).
- Each unit has a `name` and a `symbol` (1 to 10 characters, **unique in the business** ignoring
  case, so `kg` is never ambiguous). Exactly one unit per category is the **reference**: every
  quantity of an item based on it is stored as an **integer number of thousandths of the
  reference**.
- Every other unit is a **fraction of the reference**: one unit = `ratio_num / ratio_den` reference
  units, whole numbers from 1 to 1,000,000,000, returned reduced (`2000/2` comes back `1000/1`).
  Show "bigger" or "smaller" by comparing them; it is not a field. **Never go through a float**: when
  the owner types `236.588`, build `236588/1000` from the text.
- `rounding_scaled` is the unit's **step** in thousandths of that unit (`10` is 0.01, `1000` whole
  units). A quantity that is not a multiple of its unit's step is refused, never rounded (BR-UOM-06).
  The standard steps are 0.01 for `kg` and `L` and whole units for `g`, `ml` and pieces.
- **Creating** a category: `{name, units: [{name, symbol, is_reference?, ratio_num?, ratio_den?,
  rounding_scaled?, active?}]}`; exactly one unit has `is_reference: true` and a ratio of 1/1.
  Defaults: ratio 1/1, `rounding_scaled` 10, `active` true.
- **Changing** a category: `units` in a `PATCH` lists only what changes. A unit with an `id` is
  replaced by what you send (send all its fields), a unit without one is added, units left out stay.
  Units are deactivated, never deleted. The reference stays the reference, at 1/1 and active.
- **A unit an item uses keeps its ratio** (BR-UOM-07): changing it answers `409 conflict`. Add a new
  unit and deactivate the old one. Show the conversion ("1 sdm = 15 g") before saving, since a
  wrong ratio cannot be fixed in place once used.
- No conversion between categories: an item bought by the litre and used by the gram gets a pack.

**Stock items** (BR-ITM, BR-UOM-04, 05):

- `{name, type, category_id, base_uom_id, recipe_uom_id?, track?, min_stock_scaled?,
  shelf_life_days?, description?, packs?}`. `type` is `ingredient` (bahan baku), `supporting` (bahan
  penolong), `prepared` (setengah jadi), `finished` (barang jadi) or `supply` (perlengkapan).
  Equipment is not a stock item.
- `category_id` is required and must not be archived. `base_uom_id` is a **reference unit** (`g`,
  `ml`, `pcs` or one of your own); quantities of the item are in thousandths of it. `recipe_uom_id`,
  if set, is an active unit of the same category that recipes are written in (null: the base unit;
  `clear_recipe_uom: true` goes back to it).
- `track` (default true): an untracked item has no balance, no opname and no minimum stock.
  `min_stock_scaled` is the reorder level in thousandths of the base unit, on the base unit's step;
  `0` clears it, and turning `track` off clears it too. `shelf_life_days` 1 to 3650; `0` clears it.
- **Packs** are the item's own packaging: "Beras: 1 karung = 25 kg" is `{name: "karung",
  ratio_num: 25000}` on an item based on `g` (one pack = `ratio_num / ratio_den` base units).
  `rounding_scaled` is the step in thousandths of a pack (default 1000, whole packs). A pack keeps its
  ratio forever (BR-UOM-07); in a `PATCH`, a pack with an `id` is replaced by what you send with its
  ratio unchanged (`400` otherwise), packs without one are added, packs left out stay. Packs come
  back from small to large.
- The base unit cannot change while the item has packs, which are measured in it (`409`). From the
  stock ledger (B3.3) on it will not change once the item has any movement either (BR-ITM-07).

## 4. The POS: startup, then the sync loop

The POS works **fully offline**. The server is a place to send what happened and to learn what
changed; it is never in the way of ringing up a sale.

```
pair (once)  →  token  →  pull (snapshot)  →  work offline: shift, sales, voids…
                  ↑                                 │ events go to a local outbox
                  └──────  push outbox, pull changes, whenever online  ──┘
```

### 4.1 First run

1. A manager signs in on the tablet (`/v1/auth/login`) and calls `POST /v1/devices/pair`. Store the
   `device_secret` securely. The manager's own session can then be dropped.
2. `POST /v1/devices/token` `{device_secret, app_version?, client_time?}` → `DeviceSession`
   (`access_token`, `device_id`, `outlet_id`, `tenant_id`). Renew when it expires or on `401`; a
   `403 device_revoked` here means stop for good. Always send `client_time` so the server can warn
   the owner about a wrong clock.
3. `GET /v1/sync/pull` with no cursor returns a **snapshot** (section 4.3) that includes the outlet,
   its settings, the catalog, the roster and the entitlements. The tablet is ready to trade.
4. `GET /v1/pos/roster` is an alternative way to fetch the device record, outlet and staff; the pull
   already carries the same information, so normally you only use pull.

### 4.2 Push: sending events

`POST /v1/sync/push`

```json
{
  "device_id": "…",
  "client_time": "2026-10-04T08:30:00Z",
  "app_version": "1.4.2",
  "unsynced_events": 0,
  "oldest_unsynced_at": null,
  "events": [ { "id": "…", "idempotency_key": "…", "type": "sale.completed",
                "staff_id": "…", "device_time": "…", "schema_version": 1, "payload": { … } } ]
}
```

* Send **oldest first**, at most **500 events and 1 MB** per call; page through the outbox.
* `unsynced_events` = how many events remain in the outbox **after** this push, and
  `oldest_unsynced_at` the device time of the oldest (required when the count is above 0). The
  server emails the owner when events stay stuck too long, so report honestly. Pull accepts the
  same fields.
* The response has one result per event, in order: `{id, status, code?, detail?}`.

| `status` | What the POS does |
|---|---|
| `accepted` | Done. Delete it from the outbox. With `code: "pending_dependency"` the server parked it until a record it refers to arrives (for example a void that got ahead of its sale); **still delete it**, the server will apply it by itself. |
| `duplicate` | Already accepted earlier (a lost response). Delete it. Nothing was applied twice. |
| `rejected` | Malformed, will never be applied. **Do not resend.** Move it to a "problem events" list, keep it for support, and tell the user. Codes: `unknown_type`, `unsupported_schema_version`, `unknown_staff`, `wrong_outlet`, `malformed`, `invalid_payload`, `invalid_receipt_number`, `duplicate_receipt_number`, `unknown_reference`, `invalid_value`, `already_voided`, `already_refunded`, `already_closed`, `idempotency_conflict`, `id_conflict`, `device_revoked`, `abandoned`. `detail` says which field. |
| `retry` | A server fault; nothing was recorded. Keep it and send again later with backoff. |

Rules that matter:

* **Pushing the same event again is always safe.** Retry freely after a timeout or a lost response.
* The same `idempotency_key` with *different content* is `rejected: idempotency_conflict`. Never edit
  an event after creating it; to correct something, send a new event (for example a void).
* **Only malformed events are rejected.** A sale with odd totals, a stale price, or by someone
  without the permission is accepted and flagged, because the customer has already paid. So the POS
  must never refuse to complete a sale because it thinks the server might object.
* `403 device_revoked` on the whole push: nothing was accepted; keep the outbox.
* `400 validation_failed` on the push itself (for example `device_id` not matching the token, or
  more than 500 events): fix and resend.

### 4.3 Pull: receiving changes

`GET /v1/sync/pull?cursor=…&limit=…&client_time=…&app_version=…&unsynced_events=…&oldest_unsynced_at=…`

The response carries the **current state of every entity that changed** since your cursor, not a
log of edits, so apply it as an upsert by `id`:

| Field | Apply as |
|---|---|
| `suspended` | Every response. `true` while an operator has suspended the business: see section 5. |
| `outlet` | Present when the outlet or its settings changed (and in a snapshot). Replace the local outlet and settings. |
| `categories`, `kitchen_stations`, `items` (with `variants` and `modifier_group_ids`), `modifier_groups` (with `modifiers`) | Upsert by id. **Archived ones are included, marked with `archived_at`**: keep them (an old sale may need the name), but do not offer them for new sales. |
| `outlet_variants` | This outlet's price override and availability, upsert by `variant_id`. In a snapshot, a variant not listed is at base price and available. |
| `staff` | Roster entries that changed (`display_name`, `pin_hash`, `permissions`). Upsert. |
| `removed_staff_ids` | Remove these from the local roster. |
| `deleted` | Entities removed outright. Always empty for now (the catalog is archived, not deleted); handle it anyway. |
| `entitlements` | The plan snapshot with `expires_at`. Apply at the next sync, **never in the middle of a shift**. Until `expires_at`, keep applying it if the server is unreachable. |

Paging and the cursor:

* **Store the returned `cursor` only after you have applied the whole response**, in one local
  transaction. Send it next time.
* While `has_more` is true, pull again immediately.
* `snapshot: true` means "this is the whole state": **replace** the local copy (do not merge). You
  get one when you have no cursor, or the server could not use yours (unreadable, or ahead of the
  server, for example after a restore). Never fail on it; just replace. The outbox is unaffected.
* The cursor is opaque. Never parse or compare it.

The catalog of a different outlet never reaches this device; pulls are filtered to the device's own
outlet.

**Pull before you push, or push before you pull?** Push first (the money), then pull. Both can run
whenever the tablet is online: on app start, after each completed sale, on a timer (a minute or two
while a shift is open), and when connectivity returns.

---

## 5. Offline rules for the POS

* **Staff PIN**: the roster gives each person's `pin_hash` (argon2id, PHC format) and `permissions`
  at this outlet. Verify the PIN locally and enforce permissions in the UI. This is a convenience
  switch, not a security boundary: the server re-checks permissions when events arrive and flags
  violations. A PIN of someone with no hash yet (`pin_hash: null`) cannot sign in.
* **Permissions to enforce on the tablet**: `sale.create` to ring up, `discount.apply_manual` for a
  manual discount (or a manager's approval, named in `approved_by`), `sale.void` to void and `sale.refund` to refund (or approval),
  `shift.open`, `shift.close`, `drawer.open_no_sale` for pay-ins, pay-outs and no-sale openings.
* **Rung-up prices come from the local catalog**, the one at your `catalog_seq` (section 6.4). If a
  price changed on the server after your last pull, the sale is accepted and shows `stale_price`:
  expected, not an error.
* **Entitlement limits** are enforced on the server at pairing and staff creation. The tablet only
  needs to hide modules whose key is `0`.
* **Clock**: events carry `device_time`. A tablet clock that is wrong moves sales to the wrong
  business day. Show a warning when the device clock is clearly off (compare `server_time` from
  responses), and always send `client_time`.
* **Suspended business** (`suspended: true` in a pull): show a banner, let the shift that is open
  run to its close, and refuse to open a new one. Keep pushing: the server accepts every event
  while suspended, so nothing rung up is lost, and token exchange keeps working. When a later pull
  says `suspended: false`, carry on as normal.
* **Never lose the outbox.** It lives only on the tablet until accepted. Do not wipe app data or
  log the device out while it is non-empty.

---

## 6. The events

Five event types, all `schema_version: 1`. Unknown extra fields in a payload are ignored, so a
newer app can add fields without breaking an older server.

### 6.1 Envelope

| Field | Rule |
|---|---|
| `id` | UUIDv7 generated on the device. For a create event (`shift.opened`, `sale.completed`) it **is the id of the record created**: the sale id, the shift id. |
| `idempotency_key` | A UUID. Equal to `id` for create events. For a repeated operation, generate it once and reuse it on every retry. |
| `type` | One of the five below. |
| `staff_id` | Who did it on the tablet (a roster id). Must belong to this business. |
| `device_time` | When it happened by the device clock, RFC 3339. |
| `schema_version` | `1`. |
| `payload` | Object, per type. Kept exactly as sent. |

### 6.2 Payloads

All amounts are integer rupiah (non-negative unless stated).

**`shift.opened`** `{ opening_cash }`. The event id is the shift id.

**`shift.closed`** `{ shift_id, counted_cash }`. A shift closes once (`already_closed`).

**`cash.movement`** `{ shift_id, kind, amount, reason }`: `kind` is `pay_in`, `pay_out` or
`no_sale`. `no_sale` has no amount. `pay_out` needs a `reason`.

**`sale.completed`**

```json
{
  "shift_id": "…",
  "receipt_number": "JKT1-03-000482",
  "catalog_seq": 1234,
  "pricing": { "version": 1, "price_includes_tax": false, "tax_rate_bp": 1100,
               "service_charge_rate_bp": 500, "service_charge_taxable": true,
               "cash_rounding_unit": 100, "cash_rounding_mode": "nearest" },
  "lines": [ { "variant_id": "…", "name": "Latte (Large)", "unit_price": 28000, "quantity": 2,
               "modifiers": [ { "modifier_id": "…", "name": "Oat milk", "price_delta": 5000 } ],
               "discount": 0, "allocated_bill_discount": 0, "total": 66000 } ],
  "discounts": [ { "line": 0, "kind": "percent", "value": 1000, "amount": 6600,
                   "reason": "staff", "approved_by": "…" } ],
  "totals": { "subtotal": 66000, "discount_total": 6600, "service_charge": 2970,
              "tax": 6806, "rounding_amount": 4, "total": 69176 },
  "payments": [ { "method": "cash", "amount": 69176, "tendered": 70000, "change": 824, "reference": "" } ]
}
```

* `receipt_number` is `{outlet_code}-{device_code, at least 2 digits}-{counter, 6 digits}`, for
  example `JKT1-03-000482`. The device code is yours (from the device record); the counter is a
  per-device counter that you keep and increment locally, **never reuse and never skip back** (a
  gap, from a voided or unsent draft, is fine). A wrong prefix is `invalid_receipt_number`; a
  repeat is `duplicate_receipt_number`.
* `catalog_seq`: the catalog version your local copy was at when ringing the sale up. Taken from the
  last pull's state; the server uses it to tell a stale price from a deliberate one.
* `pricing`: the **exact settings you priced with**, copied from the outlet settings you hold.
  Always send them; they let the server re-check the sale even if settings change later.
  `pricing.version` is the algorithm version (`1`).
* `lines[].name` is the name **as printed on the receipt**, stored with the sale so later renames
  do not change history. Include the variant name when it is not empty.
* `discounts[].line` is the index into `lines`, or omitted/`null` for a discount on the whole bill.
  `kind` is `percent` (`value` in basis points: `1000` = 10%) or `amount` (`value` in rupiah);
  `amount` is the rupiah it came to. A line may have only one discount.
* `payments[].method`: `cash`, `qris_manual`, `qris_dynamic`, `ewallet`, `card_manual`. `amount` is
  what was applied to the bill, **net of change**. For cash send `tendered` and `change`. At most
  10 payments. The payments must add up to `totals.total + totals.rounding_amount` (the rounding is
  0 unless the bill is paid entirely in cash), and for a cash payment `tendered − change` must equal
  `amount`; otherwise `payment_mismatch` is raised.
* Limits: 1 to 200 lines, quantity 1 to 10,000, unit price up to 1,000,000,000, up to 50 modifiers
  per line, text up to 200 characters.

**`sale.voided`** `{ sale_id, reason, approved_by?, shift_id? }`. `shift_id` is the shift the void
happened in (default: the sale's own shift). `approved_by` is a manager who approved it, if the
voider lacks `sale.void`. A sale voids once (`already_voided`). It may arrive before its sale; the
server parks it (`pending_dependency`) and applies it when the sale arrives. A sale that has a
refund cannot be voided (`already_refunded`): refund the rest instead.

**`refund.issued`** `{ sale_id, shift_id, method, reason, approved_by?, lines: [{ line_no, quantity, amount }] }`.
Money given back for some or all of a completed sale; the event id is the refund's id. `shift_id`
is the shift open now (a cash refund leaves its drawer); `method` is how the money went back (`cash`,
`qris_manual`, `card_manual`, `ewallet`; `qris_dynamic` is recorded but the gateway refund itself
arrives with the gateway). `lines` names each sale line by its `line_no` with the units and rupiah
returned; the refund's amount is the sum of the lines.

* **You decide the amounts**, and the server records them as sent. Suggested rule: a line's share of
  what the customer paid, `round_half_up(line.total × (sale.total + sale.rounding_amount) / net)`
  for the whole line, where `net` is the sum of the line totals, then the same share per unit for a
  part of the line; show it to the cashier before confirming.
* The server flags `refund_over_quantity` when the sale's refunds give back more units of a line than
  were sold, and `refund_over_paid` when they give back more money than the sale took. It never
  rejects a refund for that.
* Needs `sale.refund` (Owner and Manager by default) from the cashier or the `approved_by` manager;
  otherwise `permission_missing`. Do not offer refund on a voided sale (`already_voided`), and do not
  offer void once a sale has a refund.
* It may arrive before its sale or shift; it waits (`pending_dependency`) like a void. A line number
  that is not on the sale, a line listed twice, no lines, or a quantity of 0 is `invalid_payload`.

### 6.3 Ordering

Events may reach the server out of order (two tablets, a retry, a long-offline day). The server
handles it: a close, movement or sale waits for its shift; a void or refund waits for its sale. You do not
need to order across event types, but **do** send oldest first and do not hold back events
because an earlier one was parked.

### 6.4 What the server does with a sale

It stores the sale **exactly as you sent it**, recomputes it with the same algorithm (section 7),
and raises flags for any difference. So the receipt the customer got and the report the owner sees
always agree, even when the app has a bug; the flag tells a human where to look.

---

## 7. Pricing: one calculation, two implementations

The POS prices bills locally, offline; the server recomputes them. If the two disagree, every such
sale is flagged `total_mismatch`. The shared definition is `internal/pricing/pricing.go`
(package comment) and `docs/BACKEND_PLAN.md` section 4.8, and the check is the golden vectors in
`testdata/pricing-vectors/` (`README.md` there has the file format). **Run every vector in the POS's
CI**; a change to the algorithm is a new `pricing.Version`, never a quiet edit.

Summary (see the plan for the full text):

* A line is `(unit_price + sum of modifier deltas) × quantity`, less its own discount.
* A **bill discount** applies to the sum of the lines after their own discounts and is spread over
  the lines in proportion, by largest remainder, ties to the earlier line.
* **Service charge** and **tax** are computed once per bill (not per line) from the discounted
  subtotal. With `price_includes_tax` the tax is extracted (`base × rate / (10000 + rate)`) and
  shown, but the total does not include it again. The service charge is added either way.
* **Rounding is half up, away from zero** everywhere, except cash rounding with mode `down` or `up`.
* **Cash rounding** applies only when the bill is paid **entirely in cash**; it is
  `rounding_amount` (cash total − total) and never changes `total`.
* Use integer arithmetic (`BigInt` or checked 53-bit): products like `amount × rate_bp` are exact
  for the vectors, but the server limits go beyond 2^53 in principle.
* Bills that cannot be priced (two discounts on a line, a discount larger than its line, a quantity
  of 0) are refused by the algorithm with `discount_exceeds_amount`, `invalid_discount`,
  `duplicate_discount`, `invalid_quantity` or `negative_price`: the POS UI must prevent them.

`GET /v1/pos/receipt-test` (device token) returns a made-up bill priced with the outlet's real
settings, for checking printer layout and the calculation; it is not a sale.

---

## 8. Putting it together: what each screen calls

| Screen | Calls |
|---|---|
| Sign up | `auth/signup` → "check your inbox" (`auth/resend-verification`) → link → `auth/verify-email` → sign in |
| Back office sign in | `auth/login` → `me` → `entitlements` → `announcements` |
| Dashboard / end of day | `reports/days/{date}?outlet_id=` |
| Sales reports (charts, best sellers, payment mix, CSV download) | `reports/sales?outlet_id=&from=&to=&group_by=day\|item\|payment_method[&format=csv]` |
| Shift detail | `reports/shifts/{shiftId}` (ids come from the day report's `shifts[]`) |
| Sales list and search | `sales?…` → `sales/{saleId}` |
| Catalog manager | `categories`, `kitchen-stations`, `items`, `modifier-groups`, `outlets/{id}/variants` |
| Inventory setup | `expense-types`, `stock-categories`, `uom-categories`, `stock-items` (only when `module.inventory` is on) |
| Menu import | `catalog/import?dry_run=true` → show counts and row errors → `catalog/import` |
| Staff manager | `roles`, `staff`, `staff/{id}/pin` |
| Devices | `devices`, `devices/pair`, `devices/{id}` (revoke) |
| Outlet settings | `outlets`, `outlets/{id}/settings` |
| POS pairing | `auth/login` → `devices/pair` |
| POS runtime | `devices/token`, `sync/pull`, `sync/push` |

## 9. Device health (back office)

Each device in `GET /v1/devices` carries: `last_seen_at`, `last_sync_at`, `app_version`,
`clock_skew_ms` (server time minus device time; positive means the device clock is **behind**),
`unsynced_events`, `oldest_unsynced_at` and `health_reported_at`, all taken from what the POS
reports on push, pull and token exchange. Show a warning when `unsynced_events` is above 0 and
`oldest_unsynced_at` is old, or the skew is more than a few minutes. The server also emails the
owners once per incident (by default after 30 minutes undelivered, or 3 hours silent during an open
shift).

## 10. Not built yet

So you do not wait for it or invent it: stock levels and movements, recipes, purchasing, stock
opname, waste and transfers (inventory setup is in 3.7), kitchen display (and
ticket status; kitchen tickets themselves are printed by the POS, see 3.5),
customers, loyalty, gateway refunds, payment gateways (QRIS dynamic, e-wallets: the methods
exist as labels only), receipt printing endpoints, file/image upload (an item's `image_url` is a
plain URL you host), CORS, and webhooks. The roadmap is in
`docs/BACKEND_PLAN.md`.

## 11. Local development

```sh
make db-up && make run           # API on :8080
make worker                       # background jobs; locally, emails go to the log
./bin/orion admin seed-demo       # demo business, menu, cashier PINs, a paired device
```

`seed-demo` prints the demo owner login and the cashier PINs. Regenerate your client from
`api/openapi.yaml` after every pull of the backend.

## 12. The operator console (`/admin`, Orion staff only)

Not for the business apps. Sign in with `POST /admin/auth/login`, then `POST /admin/auth/totp/verify`
with a code; the session lasts an hour (no refresh: sign in again). Every change takes a `reason`
(1 to 500 characters) and is written to the platform audit log with the operator, address and
user agent; a missing or blank reason is `400`.

| Call | Notes |
|---|---|
| `GET /admin/tenants?q=&cursor=&limit=` | Businesses by id: plan, `subscription_status`, `suspended_at`, outlets, active devices, `last_sync_at`, and `sales_7d`/`events_7d` from the nightly metrics (up to a day behind). `q` matches name or slug. |
| `GET /admin/tenants/{tenantId}` | `tenant`, `entitlements[]` (value in force, `source`, default, plan value, and the override with `in_force: false` once expired), `devices[]` (last sync and contact, app version, clock skew, undelivered events, open alerts), `metrics[]` (last 30 days, oldest first). Not audited. |
| `POST /admin/tenants/{tenantId}/suspend`, `/reinstate` `{reason}` | Suspension makes the business read-only and tells its tablets to finish the open shift; see `tenant_suspended` and section 5. `409` if it already is. |
| `PUT /admin/tenants/{tenantId}/plan` `{plan, reason}` | `free` or `early_access`. `409` if already on it. |
| `PUT /admin/tenants/{tenantId}/entitlements/{key}` `{value, expires_at?, reason}` | An override for one business; `value: null` clears it. Unknown key `404`. |
| `GET /admin/entitlement-keys`, `PATCH /admin/entitlement-keys/{key}` `{default_value, reason}` | Every key with its default and per-plan values. Only a **flag**'s default can be changed (a rollout to everyone); modules and limits are `400`. |
| `POST /admin/devices/{deviceId}/revoke` `{reason}` | For a lost or stolen tablet. The business sees it in its own audit log as done by Orion support. |
| `GET /admin/devices/stopped-syncing?quiet_minutes=` | Tablets in use, across businesses, silent for longer than that (default 1440). |
| `GET /admin/audit-log?tenant_id=` | Newest first; `tenant_id` narrows it to one business. |
| `GET/POST /admin/announcements`, `POST /admin/announcements/{id}/end` `{reason}` | Publish to every business (no `tenant_id`) or one: `severity` (`info`, `warning`), `title` and `body` as `{id, en?}` (Indonesian required), optional `starts_at`/`ends_at`. Ending one takes it down now; the list shows the latest 200, ended ones included. |

Changes to a plan, an override or a flag reach the business within 30 seconds at most (in this
server process, at once).
