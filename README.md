# Orion POS backend

Go API for Orion, a point-of-sale system for cafes and restaurants in Indonesia.

This repository is at the planning stage. Start with the
[backend implementation plan](./docs/BACKEND_PLAN.md), which follows the product
[roadmap](https://github.com/Orion-POS/Inventory-React/blob/dev/docs/ROADMAP.md) and the
[architecture decision records](https://github.com/Orion-POS/Inventory-React/tree/dev/docs/adr).

In short: one Go binary on PostgreSQL (modular monolith), an OpenAPI-first contract, an
offline-first sync protocol for the cashier app, integer-rupiah money and an append-only stock
ledger.
