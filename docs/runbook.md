# Operational runbook

The local integration fixture exercises the paths below. Production backup/restore, provider, and role procedures still need deployment-specific rehearsal. Never mark an escrow paid from an HTTP request or pending transaction alone.

## RPC stale or unavailable

Check `settlekit_rpc_polls_total`, `settlekit_chain_checkpoint`, `settlekit_chain_lag_blocks`, `/v1/health/ready`, and JSON logs. A configured fallback is accepted only if it reports the configured chain ID and exact stored checkpoint hash; it remains active until process restart. Do not advance the checkpoint while the block/log set is incomplete. On recovery, replay from the durable checkpoint and dedupe by log identity. If providers disagree, stop reconciliation and investigate the canonical chain manually.

## Deep reorg or stuck checkpoint

At a fork deeper than 64 blocks, no common ancestor, or more than 1,000 affected intents, indexing stops and readiness fails. Preserve the DB snapshot and RPC evidence. Identify a trusted ancestor, then execute an explicit reviewed rollback/replay procedure; do not delete payment history ad hoc. The local Anvil fixture exercises snapshot/revert; PostgreSQL regressions additionally exercise A -> B -> A, release/refund reversal, lost confirmations and restart after an interrupted reconciliation transaction. Returning blocks/logs retain immutable identity and audit history. A manual deep-fork reconciliation CLI is not implemented; this remains a deployment limitation.

## Webhook receiver outage

Inspect `outbox_events` (`delivered_at`, `dead_lettered_at`, `attempts`, `next_attempt_at`, `lease_until`) and `settlekit_webhook_attempts_total`. Retried events retain event ID and body; receivers dedupe by event ID. After a permanent 4xx or 10 attempts (including an uncertain abandoned final attempt), correct the endpoint/secret, then run `SETTLEKIT_DATABASE_URL=... go run ./cmd/settlekit-replay --event-id <UUID>`. The command only accepts a dead-lettered, undelivered event and preserves its ID/body. Never synthesize a new business transition just to resend. If `attempts=10` has no final flag after a crash, keep the dispatcher running: after the 30-second lease expires it marks the row dead-lettered without sending. Then use the same replay command. An unknown attempt may already have been processed; receiver deduplication is mandatory. Events can arrive out of order; use status versions and reversal references.

## Emergency pause and credential rotation

`PAUSER_ROLE` pauses new funding and release only; refunds remain possible. Pause cannot reverse prior payments, stop token transfers outside escrow, or undo a compromised admin grant. Admin alone unpauses. Rotate merchant/operator bearer tokens and webhook secret through environment/secret-store configuration using a coordinated cutover with the receiver; the service accepts one value of each at a time. Ensure in-flight webhook attempts can be verified during the cutover. Keep old secrets out of logs.

## Database recovery

Restore from a tested PostgreSQL backup, validate schema and canonical checkpoint, then replay chain data from a conservative ancestor. Recompute derived states and outbox in a transaction. Do not resume webhook delivery until reconciliation finishes. A production backup/replay exercise remains a deployment prerequisite.

## Operator transaction with unknown outcome

The API returns unsigned calldata and never submits a transaction. If a wallet or RPC times out during submission, inspect the transaction hash, account nonce, and escrow state on a trusted RPC before considering another transaction. Never blindly resubmit a potentially successful non-idempotent call. The contract's terminal exclusivity provides a final on-chain guard, but a replacement transaction can still cost gas or race another operation.

## Confirmation configuration

Outside Anvil chain 31337, set `SETTLEKIT_CONFIRMATIONS` explicitly (2..128) before starting the service; missing/invalid values fail before DB/RPC access. Existing Sepolia evidence uses 6. A policy threshold is not irreversible finality. The historical Sepolia scenarios do not establish execution of the current backend revision; the retained public record does not identify a reproducible backend commit for those transactions.
