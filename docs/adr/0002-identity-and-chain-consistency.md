# ADR 0002: Escrow identity and canonical-chain policy

Status: accepted for v1, 2026-10-02.

Context: escrow IDs must resist accidental replay across contracts/chains, and payment status must survive duplicate logs and reorgs.

Alternatives for ID: caller-chosen bytes32, sequential contract counter, UUID-only hash, or domain-separated hash of UUID plus escrow terms. Choose the last. A UUID-only hash can be occupied by an unrelated payer using the visible intent UUID and different terms, denying the real payer's funding. The API generates a cryptographically random 16-byte UUID. Contract computes `keccak256(abi.encode("SETTLEKIT_ESCROW_V1", block.chainid, address(this), intentId, msg.sender, payee, amount, expiresAt))`. Backend computes and stores the same value using the expected payer and terms; exact tuple replay reverts. Different terms or payer produce a different ID and must never match the API intent. A payer can deliberately create another escrow with altered terms and the same UUID; it is an unrelated on-chain escrow and the backend must ignore it. UUID guessing is not an authorization mechanism.

Chain decision: local Anvil threshold is 2 blocks inclusive of the event block. Other networks require an explicit configured threshold rather than inheriting a universal number. Maximum automatic reorg depth is 64; deeper or ancestorless forks fail closed. Store each `(number, hash, parent_hash)`, canonical flag, unique log identity `(chain_id, block_hash, tx_hash, log_index)`, and durable checkpoint. State is derived only from canonical logs in a transaction that also updates outbox. A reorg can undo `CONFIRMED`, `RELEASED`, or `REFUNDED` in the local view, with a reversal webhook and replay. `REORGED` is transient, not a final business result. Threshold is a product risk policy, not chain finality.

Alternatives for RPC: trust first provider, or require quorum. Choose parent-hash validation and cross-check of block/log origin with the active configured RPC. The implementation can fail over to one configured fallback only when its chain ID and stored checkpoint hash match; it remains active until restart. Neither provider is treated as a consensus oracle. Conflicting data fails readiness pending reconciliation.

Expiry decision: an unfunded intent becomes `EXPIRED` only after the watcher has fully indexed a canonical head whose block time reaches the deadline. Wall-clock time alone is insufficient because a delayed watcher might later discover a valid pre-deadline fund. `EXPIRED` can enter `REORGED` and be recomputed if that head is orphaned. This favors correctness over instant expiry display.

Consequences: a deeply reorganized chain needs manual recovery; no false irreversible guarantee. Tests must include a previously observed orphan, duplicate log, and deep fork.

## Recovery refinements, 2026-10-03

A returning orphan block (A -> B -> A) reuses its stored block/log identities. Upserts change canonical/removed flags only when immutable block fields and log payloads match. A changed, missing, duplicated, or additional relevant log rolls back the entire block import. Original import timestamps, orphan evidence, state history and reversal events are retained. The checkpoint serializes imports and reconciliation; restart resumes either the old committed branch or the rewound ancestor.

The 1,000-payment bound applies to state changes: orphaned evidence, lost funding/terminal confirmations, and expiry revoked by ancestor time. Unaffected historical/unfunded intents do not consume that bound. Queries remain deadline-bounded; more than 1,000 affected payments fail atomically and require manual intervention. A lower canonical head can revoke confirmations even when the event block remains canonical.

Only chain ID 31337 has the two-confirmation default. All other chains require explicit 2..128 configuration, validated before database or RPC access. Sepolia release evidence enforces at least six confirmations separately.
