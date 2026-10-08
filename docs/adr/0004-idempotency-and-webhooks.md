# ADR 0004: Durable idempotency and signed webhook delivery

Status: accepted for v1, 2026-10-02.

Context: HTTP retries and process death can duplicate intents or lose webhook events. A receiver must distinguish an authentic event from a replay.

Idempotency alternatives: in-memory lock/TTL cache or durable DB record. Choose a unique DB key on `(principal, method, canonical concrete path, idempotency_key)` with exact raw request SHA-256 and persisted status/body. The concrete path includes the canonical lowercase intent UUID for operator actions, so one key on two different intents cannot return the wrong call. `Idempotency-Key` is 16-128 characters from `[A-Za-z0-9._:-]`, required on all mutating endpoints. Same key and body returns the original response, even after restart; same key and different raw bytes returns `409 IDEMPOTENCY_CONFLICT`. Keep records for the life of v1 data, avoiding a time-window after which a retry silently creates another intent. Business row and idempotency result commit atomically.

Webhook alternatives: best-effort send from request path or transactional outbox. Choose outbox. State revision and outbox event commit together; one bounded dispatcher leases rows. Stable `event_id` (UUID) and raw JSON body are immutable. Each attempt uses current Unix-seconds timestamp and HMAC-SHA256 of `timestamp.event_id.raw_body` in `X-SettleKit-Signature: v1=<lowercase hex>`. Headers also include `X-SettleKit-Event-Id` and `X-SettleKit-Timestamp`. Receiver verifies constant-time HMAC, five-minute clock window, and dedupes the event ID; the timestamp changes on retry, ID/body do not.

2xx marks delivered. Timeout, 408, 429 and 5xx get bounded exponential backoff with jitter; other 4xx become dead-letter immediately. Maximum 10 attempts; afterward only a documented manual replay command may requeue. No HTTP call holds a DB transaction. Reorg compensation is a new event ID referencing the reverted prior event. Store endpoint and secret references outside the payload and never log secrets.

Consequences: delivery is at least once, not exactly once. Receiver idempotency is mandatory. Tests cover body-byte signature, replay window, retry classification, restart, and concurrent workers.

## Durable attempt boundaries

Claim increments the attempt count and writes a 30-second lease atomically before HTTP. A crash before sending and a crash after sending are indistinguishable; after lease expiry, attempts below ten can retry with the same ID/body. An expired abandoned attempt ten is atomically dead-lettered without an eleventh send. A canceled or failed acknowledgement leaves the lease for this same recovery path. No detached goroutine or unbounded acknowledgement is introduced.

Manual replay of the recovered dead letter resets the attempt budget and retains event identity/body. Receivers must dedupe even if a prior request succeeded before its DB acknowledgement failed. Dead letter means automatic delivery stopped, not proof the receiver never processed the event. An active unexpired lease cannot be stolen or manually replayed. `TestOutboxCrashBoundaries`, `TestOutboxCanceledBeforeClaim`, and `TestConcurrentOutboxWorkersClaimOnce` cover these boundaries. Secret rotation remains a coordinated single-key restart/cutover procedure; no multi-key rotation test or protocol is claimed.
