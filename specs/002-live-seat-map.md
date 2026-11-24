# 002 · Live seat map

## Context
A buyer looking at the seat map should see seats disappear as others take them, without refreshing.

## Behavior
- `GET /v1/events/{id}/stream` is a Server-Sent Events stream. Every change sends `event: seats` with the seats and their new status (`held`, `sold`, `available`).
- The page at `/` shows the seat map of an event, lets you pick seats, hold, purchase and release, and updates live from the stream.
- A heartbeat comment every 15 seconds keeps proxies from closing idle streams.

## Acceptance criteria
- A hold made by one client reaches the stream of another client.
- A slow subscriber never slows down the others or the booking path: each subscriber has a small buffer, and a subscriber that falls behind is disconnected (the page reconnects and reloads the map).
- When the client disconnects, its subscription is removed (no goroutine or memory leak).

## Out of scope
WebSockets, multiple server instances (they would share changes through a broker such as Redis or NATS).
