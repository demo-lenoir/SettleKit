# ADR 0001: Escrow state, permissions, token, and expiry

Status: accepted for v1, 2026-10-02.

Context: `SPEC.md` defines create/fund/release/refund/expire, roles, and pause. The precise transitions, expiry boundary, and paused refund behavior affect custody.

Alternatives: (a) separate create and fund, allowing unfunded on-chain records; (b) atomic create-and-fund. Choose (b) to make the funded record and token balance inseparable. A failed transfer leaves no escrow or events. (a) remains a future option only with an explicit underfunded state and recovery model.

Decision: `NONE -> FUNDED -> RELEASED | REFUNDED`; no partial payout. Only the payer funds. Operator releases to the fixed payee before expiry or refunds to the fixed payer at any time while funded. At and after expiry, any caller may trigger refund to payer. `EscrowExpired` and `EscrowRefunded` are emitted together for that path. Exact boundary is `block.timestamp >= expiresAt` for refund and `< expiresAt` for release. Expiry is immutable, future, and at most 30 days at execution.

Pause alternatives: block all transfers, or block new funding and release while keeping refund available. Choose the second so pauser cannot trap funds; operator refunds and permissionless expired refunds remain open. Only admin unpauses. `PAUSER_ROLE` cannot withdraw. Default admin uses OpenZeppelin delayed two-step admin transfer and is separate from routine operator. No rescue or upgrade mechanism. One immutable standard USDC-like token; exact received balance check rejects fee-on-transfer. Rebasing, callback, and malicious tokens are unsupported. A token blacklisting the contract/payer may still strand funds and requires manual token-issuer action; no arbitrary rescue is added.

Consequences: contract API is small, but recipients depend on immutable creation fields. Operator compromise still permits a release to the declared payee before expiry; it cannot redirect funds. Admin compromise can grant roles and remains a severe risk despite transfer delay. Tests must exercise all transitions, boundary timestamps, pause policy, and adversarial token behavior.
