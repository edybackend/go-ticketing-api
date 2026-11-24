# 001 · Seat holds and purchases

## Context
When tickets for a show go on sale, many people click the same seats at the same second. Nobody may be sold a seat twice, and a seat someone abandoned in their cart must go back on sale by itself.

## Behavior
- `GET /v1/events` and `GET /v1/events/{id}/seats` (each seat: `available`, `held` or `sold`).
- `POST /v1/events/{id}/holds` with `seats` (e.g. `["A1","A2"]`, 1–6 seats) and `customer` holds the seats for 5 minutes: `201` with the hold id and `expiresAt`.
- **All or nothing:** if any seat is held or sold, nothing is held: `409` with the seats that were not available.
- `POST /v1/holds/{id}/purchase` turns a valid hold into an order: `201`. Purchasing the same hold again returns the same order (`200`): safe to retry.
- `DELETE /v1/holds/{id}` releases the seats.
- A hold whose time is up no longer blocks its seats, even before the sweeper runs; purchasing it returns `410 Gone`.
- A background sweeper releases expired holds every few seconds.

## Acceptance criteria
- 200 concurrent requests for the same seat: exactly one `201`.
- Overlapping requests (`A1,A2` and `A2,A3`): one wins; the loser holds nothing (A3 stays available).
- Unknown seats or events → `404`; more than 6 seats, duplicates or no customer → `400`.

## Out of scope
Payment (see the payments-idempotency repository), seat pricing tiers, waiting rooms.
