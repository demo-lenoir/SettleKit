# ADR 0003: Operator transaction model and HTTP authentication

Status: accepted for v1, 2026-10-02.

Context: server signing introduces private-key custody, nonce allocation, uncertain submission outcomes, and an additional recovery service. Operator HTTP access alone must not authorize on-chain custody changes.

Alternatives: (a) backend signs and sends via a KMS/HSM adapter; (b) backend stores an operator action and returns unsigned calldata for an external operator wallet. Choose (b) for v1. The `/release` and `/refund` endpoints require a distinct operator bearer token of at least 32 bytes and persist the requested operation, then return `to`, `chain_id`, `data`, and `value=0`. The wallet validates the request, signs, selects nonce, and submits. The backend never holds an operator key or calls `eth_sendRawTransaction`. The contract role is the decisive authorization gate. A queued API action is not a payment status; only canonical confirmed events change status.

Unknown outcome: if the wallet/submission call times out, do not assume failure or create new calldata. The operator wallet must inspect its nonce/tx hash and escrow state before resubmission. An exact HTTP idempotency replay returns the same unsigned call. Repeated on-chain release/refund reverts after the first success, so it cannot transfer funds twice; duplicate gas spending remains possible and is a documented operational risk. There is no backend nonce manager because there is no backend signer.

HTTP authentication: all payment endpoints require a merchant bearer token; operator endpoints require a distinct operator bearer token and principal identity. Tokens arrive via environment/secret store, compare in constant time, are never logged, and may be rotated through a deployment procedure. This is suitable for local/controlled demo; internet-facing multi-tenant use would require a stronger identity provider, scoped authorization, and token lifecycle. Body/rate limits and TLS at the edge are required outside loopback.

Consequences: no backend key exfiltration or nonce races. Demo must explicitly show the operator wallet transaction. This deviates from the master architecture drawing's ambiguous API-to-contract submit arrow: wallet submission is inserted to avoid server custody, while watcher and API functionality remain in scope.

Action conflict policy: the first authenticated operator action for an intent is stored with a unique `intent_id`. A different release/refund request, even with another idempotency key, receives `409 OPERATION_CONFLICT` while the first transaction's external-wallet submission outcome may be unknown. There is no automatic cancellation or replacement endpoint in v1. This favors avoiding contradictory instructions; if the wallet never submits, the payer can still use permissionless expiry refund after the deadline. The backend never retries or rebroadcasts the wallet transaction.
