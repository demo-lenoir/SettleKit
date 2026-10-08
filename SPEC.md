# SettleKit v1 specification

This specification defines the SettleKit v1 scope. Sepolia deployment and historical release/refund evidence are recorded in `docs/evidence.md`; ADRs record the protocol and operating decisions.

## Purpose

A merchant can create a stablecoin payment intent, receive an escrow deposit from a payer, see its status after a configured number of canonical-chain confirmations, and receive a signed webhook. An authorized operator can request release or refund. The service must recover from duplicate logs, process restarts, and chain reorganizations without inventing a second payment.

## v1 boundary

MUST: payment intent API, a single USDC-like ERC-20 escrow contract, Anvil watcher, configurable confirmations, deterministic reorg reconciliation, full release and refund, transactional outbox with signed webhooks, useful metrics/traces/logs, PostgreSQL state, local reproducible demo, testnet evidence, and every verification gate in this specification.

SHOULD: primary/fallback RPC health and failover, manual webhook replay, reconciliation CLI, gas snapshot, and emergency pause. Pause is included in v1 because its policy changes fund safety. Primary/fallback failover, manual webhook replay, and gas snapshot are implemented locally; a manual deep-reorg reconciliation CLI is not implemented.

NON-GOALS: fiat/cards, accounting ledger, KYC, cross-chain support, partial releases, arbitrary token support, arbitrary rescue, contract upgradeability, a message broker, and a UI.

## On-chain protocol

- One immutable ERC-20 token address is set at deployment. It must be a standard USDC-like token: no transfer fees, rebasing, callbacks, or balance changes outside transfers. Amount is base units, never floating point; `1 <= amount <= type(uint128).max`.
- `createAndFund(intentId, payee, amount, expiresAt)` is called by the payer and atomically records and funds one escrow. It emits `EscrowCreated` and `EscrowFunded`. `intentId` is a random 16-byte UUID created by the API. The contract computes `escrowId = keccak256(abi.encode("SETTLEKIT_ESCROW_V1", block.chainid, address(this), intentId, msg.sender, payee, amount, expiresAt))`; exact replay reverts. A different tuple yields a different escrow ID and is not a match for the API intent.
- The payer must be nonzero by EVM construction; the payee must be nonzero and distinct from the payer. Expiry must be strictly in the future and no more than 30 days away at execution. A failed or undercredited token transfer reverts the entire creation.
- API `expires_at` is UTC with whole-second precision; fractional seconds are rejected. The backend converts it to Unix seconds and derives the exact expected escrow ID. The watcher accepts a funding event for an intent only if chain ID, contract, token, escrow ID, payer, payee, amount, and expiry all match the stored intent. Mismatch is an observable anomaly, never a paid intent.
- Each intent stores its exact escrow contract address; a later process configuration change cannot redirect a read or operator call to a different deployment.
- States are `NONE -> FUNDED -> RELEASED | REFUNDED`. Only `OPERATOR_ROLE` can release a funded escrow, strictly before expiry. Only `OPERATOR_ROLE` can issue an early refund. Once `block.timestamp >= expiresAt`, anyone can call `claimExpiredRefund`; funds always go to the recorded payer. That call emits `EscrowExpired` and `EscrowRefunded` in the same transaction. Terminal states never change.
- `PAUSER_ROLE` can pause creation and release. `DEFAULT_ADMIN_ROLE` alone can unpause and manage roles, with delayed two-step admin transfer. Operator refunds and expired refunds remain callable while paused so pause cannot trap funds. No rescue function exists. Admin has no direct withdrawal power.
- On every terminal transition the entire recorded amount goes to the designated payee or payer. No partial release. Contract events identify escrow, participants, amount, and transition for reconstruction.

## Off-chain status

`CREATED -> AWAITING_CHAIN -> OBSERVED -> CONFIRMING -> CONFIRMED -> RELEASED | REFUNDED`. `EXPIRED` applies only to an unfunded intent after a fully indexed canonical head reaches the deadline; wall-clock time alone cannot prove it. It is not proof of an on-chain expiry transaction and can be reorged/recomputed. `FAILED` applies to a permanently invalid intent, never to a transient RPC error. `REORGED` is a technical reconciliation status that is recomputed into `AWAITING_CHAIN`, `OBSERVED`, `CONFIRMING`, or `CONFIRMED`. A reorg can revoke a previously confirmed or terminal observation; a compensating webhook must explain the reversal. Blockchain events, not a submitted HTTP request, determine money states. `CONFIRMED` means the configured confirmation count, not irreversible finality.

The local Anvil confirmation threshold is 2 blocks including the event block. Nonlocal networks must supply their own threshold. Maximum automatic reorg depth defaults to 64 blocks. A deeper reorg, missing common ancestor, inconsistent RPC result, or database corruption stops indexing and fails readiness pending manual reconciliation. The watcher stores canonical block number/hash/parent hash, log identities, and a checkpoint atomically with derived state and outbox changes.

## API and delivery

- `POST /v1/payment-intents`: authenticated merchant request, required `Idempotency-Key`, returns UUID, escrow ID, contract address, token, expected amount, expiry, and wallet transaction data. The API does not custody payer funds.
- `GET /v1/payment-intents/{id}`: authenticated merchant status plus canonical chain evidence. Collection/list operations are outside v1; if added they require pagination and limits.
- `POST /v1/payment-intents/{id}/release` and `/refund`: separate operator authentication and required idempotency key. They return unsigned transaction data for an operator wallet. The server never signs or submits custody transactions. The watcher observes the resulting chain event. This is not a claim that the operator transaction succeeded.
- `/v1/health/live`, `/v1/health/ready`, `/metrics`: liveness, dependency/sync health, and Prometheus metrics. HTTP errors carry stable `code`, `message`, and `request_id`. Mutating requests have body and rate limits.
- Idempotency keys are scoped to principal, method, normalized route, and key. Exact request bytes are SHA-256 hashed. A replay with the same hash returns the stored status and response; a different hash returns `409 IDEMPOTENCY_CONFLICT`. The key and result are stored atomically with intent/operation creation and are retained for the life of v1 data.
- Webhooks use stable event IDs and HMAC-SHA256 over `timestamp + "." + event_id + "." + raw_body`, headers `X-SettleKit-Event-Id`, `X-SettleKit-Timestamp` (Unix seconds), and `X-SettleKit-Signature: v1=<lowercase hex>`. The receiver should reject timestamps outside a five-minute window and dedupe event IDs. Each attempt gets a fresh timestamp/signature; body and event ID stay fixed. 2xx succeeds; timeout, 408, 429, and 5xx retry with bounded exponential backoff and jitter; other 4xx are dead-lettered. After 10 attempts, manual replay is required. An abandoned final attempt becomes dead-lettered when its 30-second lease expires and the dispatcher next claims it, without another send. Attempts with uncertain outcomes may have reached the receiver; replay preserves event ID/body and requires receiver deduplication. Outbox creation shares a DB transaction with state change; network delivery occurs after commit.

## Acceptance criteria

1. Contract tests prove authorization, nonzero/invalid inputs, duplicate IDs, expiry boundary, pause policy, reverting/fee tokens, conservation, and immutable terminal states. Stateful Foundry invariant and fuzz suites run in CI; Slither findings are resolved or explained.
2. A clean local Anvil + PostgreSQL run demonstrates create, fund, observe, confirm, release or refund, and signature verification. The documented prerequisites and commands work from a clean clone in 10 minutes or less.
3. Deterministic tests prove duplicate log suppression, process restart recovery, outbox retry after receiver outage, concurrent operator requests, and reorg rollback/replay including reversal of a previously observed payment.
4. `make verify` runs formatting, vet, Go unit/race/fuzz smoke, Foundry tests/invariants, Slither, integration tests, image build, dependency/security scan, and SBOM/provenance checks. Missing tools or evidence fail the gate; `make docs-verify` checks the documentation and interface baseline.
5. Timeouts and cancellation cover outbound HTTP/RPC/DB work. Goroutines and queues are bounded and shut down cleanly. Readiness fails when dependencies or chain sync are unhealthy.
6. README links real tests, demo, architecture, threat model, runbook, limitations, testnet deployment addresses and chain, and explicitly says the contract is not independently audited. Completion requires every acceptance criterion above and the strict release gate.
