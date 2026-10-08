# Architecture

See the [payment lifecycle](../README.md#payment-lifecycle) in the README. The wallet is the only transaction signer. The API owns authenticated intent/operation requests and durable idempotency. PostgreSQL owns intent state, unique observations, canonical blocks/checkpoint, and outbox in transaction boundaries. The watcher imports ordered block headers and escrow logs with configured confirmations. The bounded dispatcher sends signed webhooks after commit.

Money state is inferred from canonical contract events. HTTP acceptance and transaction submission are only requests or observations. The system can reverse a previously confirmed off-chain status after a reorg and emits a compensating notification. Automatic reconciliation stops beyond the configured maximum depth.

`internal/indexer` fetches a bounded number of blocks per poll over bounded HTTP JSON-RPC calls. Each canonical block, relevant logs, checkpoint, derived payment versions, and outbox events commit in PostgreSQL together. A fork rewinds to a common ancestor within the configured depth and emits a reversal event that references the original event ID. The fallback RPC may take over only after it proves the same chain ID and stored checkpoint hash. It remains selected until process restart.

`internal/webhook` claims one leased outbox event at a time. Every attempt signs `timestamp.event_id.raw_body`; the body and ID remain stable across retry/restart. Permanent client errors and 10 attempts dead-letter the event. The replay CLI requeues only an undelivered dead-letter event.

The API and watcher run as bounded goroutines under one cancellation context. HTTP, RPC, PostgreSQL, and webhook operations use deadlines. Health readiness requires a live database, recent successful chain poll, and no indexed lag. Metrics use fixed labels; JSON logs omit credentials and provider URLs. The service holds no private key.
