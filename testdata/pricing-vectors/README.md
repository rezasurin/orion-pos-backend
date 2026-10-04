# Pricing golden vectors

Each `*.json` file is one bill and what it must come to. The Go implementation
(`internal/pricing`) and the POS's TypeScript implementation both run every file in CI, so the
server can recompute a sale that arrives from a device and the two never drift apart. The
algorithm is written out in the package comment of `internal/pricing/pricing.go` and in
`docs/BACKEND_PLAN.md` section 4.8; this directory is how you check an implementation of it.

The expected numbers were produced by an independent calculation in exact rational arithmetic
(Python's `fractions`), not by the Go code, and the Go code agrees with all of them.

## Format

```jsonc
{
  "name": "bill-amount-discount-even-thirds",   // unique; also the file name without its number
  "note": "why this vector exists",
  "settings": {
    "price_includes_tax": false,
    "tax_rate_bp": 1100,                 // basis points: 1100 is 11%
    "service_charge_rate_bp": 500,
    "service_charge_taxable": true,
    "cash_rounding_unit": 100,           // rupiah; 0 or 1 means off
    "cash_rounding_mode": "nearest"      // nearest (halves go up) | down | up
  },
  "lines": [
    { "unit_price": 25000, "modifiers": [5000, -1000], "quantity": 2 }   // modifiers optional
  ],
  "discounts": [
    { "line": 0, "kind": "percent", "value": 1000 },   // "line" = index into lines
    { "kind": "amount", "value": 5000 }                // no "line": a bill discount
  ],
  "tender": "cash",                      // cash | non_cash | mixed
  "expected": {
    "lines": [ { "gross": 0, "discount": 0, "allocated_bill_discount": 0, "total": 0 } ],
    "subtotal": 0, "discount_total": 0, "net": 0, "service_charge": 0, "tax": 0,
    "tax_included": false, "total": 0, "rounding_amount": 0, "cash_total": 0
  }
}
```

A vector for a bill that cannot be priced has `"expected_error": "<code>"` instead of
`"expected"`. The codes are `discount_exceeds_amount`, `invalid_discount`, `duplicate_discount`,
`invalid_quantity` and `negative_price`. An implementation must refuse such a bill, with that
code; the Go test also checks the code.

All amounts are whole rupiah integers. Use `BigInt` or checked 53-bit arithmetic in TypeScript:
intermediate products such as `amount * rate_bp` stay below 2^53 for every vector, but the
server's limits (a unit price up to 1,000,000,000, quantity up to 10,000, 200 lines) do not.

## Rules worth restating

- Rounding is half up, away from zero, everywhere except cash rounding with `down` or `up`.
- A percent discount is basis points: 1500 is 15%. An amount discount may not exceed what it applies to.
- A bill discount applies to the sum of the lines after their own discounts and is spread over
  them in proportion to those amounts by largest remainder, ties to the earlier line.
- Service charge and tax are computed once per bill, not per line, from the discounted subtotal.
- With `price_includes_tax` the tax is extracted (`base * rate / (10000 + rate)`) and shown;
  the total does not include it again. The service charge is added to the total either way.
- Cash rounding applies only when `tender` is `cash`. It is reported as `rounding_amount`
  (`cash_total - total`) and never changes `total`.

## Adding a vector

Add a file with the next number. If a vector changes the result of any existing bill, that is a
new `pricing.Version`, not an edit of these files.
