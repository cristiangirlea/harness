---
paths:
  - "internal/agent/api.go"
  - "internal/agent/messages.go"
  - "internal/agent/mcp.go"
---
# The api loop and its Messages client

- History is append-only: each assistant turn goes back exactly as received, thinking blocks
  included. Trimming, rewriting or reordering earlier messages gets the request rejected on
  models that sign their thinking. Context is reduced server-side (context editing), not here.
- The client is hand-written on `net/http` (streaming SSE, retries, prompt caching). Keep it
  stdlib; a change to SSE parsing keeps `FuzzParseSSE` passing.
- The price table (`price`, `priceOf`) is USD per million tokens. A model without a price gets
  no cost estimate and a warning, and `max_cost` cannot be enforced for it; add new models to the
  table with their published prices.
- The backend has never run against the real API (see README "Status"). Don't treat fake-server
  behaviour as confirmed API behaviour, and never run a real call to check something.
