# SettleKit threat model

Status: reviewed against the local implementation, 2026-10-03. Scope: single-chain USDC-like escrow, merchant API, operator wallet workflow, canonical watcher, PostgreSQL, outbound webhooks. This is a design and verification checklist, not an audit. Executed test status is in `docs/evidence.md` and accepted Slither findings are in `docs/security-review.md`.

## Assets and actors

Assets: payer escrow funds, operator/admin/pauser authority, merchant credentials, webhook HMAC secret, payment/chain state, DB checkpoint/outbox, RPC access tokens, CI artifacts. Actors: payer, merchant API client, authorized operator wallet, admin wallet, pauser wallet, webhook receiver, public chain users, malicious token/RPC/receiver, and compromised API client.

## Trust boundaries

1. Merchant/client -> HTTP API: input, authentication, rate/body limits, idempotency.
2. Operator client -> HTTP API -> external wallet: the API authorizes requests but never signs or submits; wallet and contract independently enforce role.
3. Payer wallet -> escrow -> ERC-20: untrusted caller and token external calls. The fixed token still needs transfer-delta and reentrancy defenses.
4. RPC -> indexer -> DB: RPC/logs are untrusted observations. Block parent linkage, canonical hash, confirmations, and a durable checkpoint establish local evidence, not universal finality.
5. DB -> dispatcher -> merchant receiver: commit before delivery; signed at-least-once events with stable IDs; receiver must dedupe.
6. CI/developer host -> deployment: dependency and artifact integrity; no real private keys in repository or default CI.

## Abuse cases, controls, residual risks, verification

| Abuse case | Control | Residual risk | Planned evidence |
|---|---|---|---|
| Unauthorized or repeated release/refund | Contract roles; terminal states; checks-effects-interactions; reentrancy guard | Compromised operator can release before expiry to designated payee, or refund to payer | Foundry auth, duplicate, invariant, reentrancy tests |
| Fee-on-transfer/rebasing token leaves insolvency | Immutable allowlisted token; exact received balance delta; explicit unsupported token assumptions | Token can change behavior or blacklist addresses | Adversarial token tests; deployment token review |
| Admin or pauser abuse | Separate roles; delayed admin transfer; pauser cannot move funds; pause never blocks refunds | Admin compromise can grant operator; delayed transfer does not prevent existing admin abuse | Role/pause tests; runbook |
| Stranger front-runs/squats an intent UUID with changed terms | Domain-separated ID binds payer, payee, amount and expiry; backend accepts only exact expected ID and event fields | A payer can create an unrelated variant escrow with the same UUID; tokens sent there are outside that intent | Same-UUID different-payer/terms test; backend mismatch test |
| Fake/duplicated/orphaned RPC logs | Contract address/topic filter, block/log identity cross-check (watcher does not fetch receipts), unique log key, parent linkage, rollback/replay | Coordinated RPC equivocation or reorg deeper than configured depth | Duplicate/reorg/deep-reorg tests |
| Payment shown final too early | Configured confirmation threshold; `CONFIRMED` explicitly reversible | Any finite threshold can be reorged | Confirmation and rollback tests; README warning |
| Client retry creates second intent | Durable idempotency records and same-transaction response | Key loss or use of a new key is a new intent | Concurrent API retry tests |
| Unknown transaction outcome leads to duplicate submission | Backend never submits; unsigned calldata is stable; wallet checks tx hash/nonce and chain state; contract rejects second terminal action | External wallet may rebroadcast or pay gas twice before learning outcome | Operator workflow integration test and runbook |
| Forged or replayed webhook | HMAC over timestamp/event ID/body; five-minute receiver window; stable event ID dedupe | Leaked secret; receiver clock skew; at-least-once duplicates | Signature/body/event-ID/time-window tests; coordinated secret rotation is an operational procedure, not a tested multi-key protocol |
| Receiver outage loses notification | Transactional outbox, bounded retries, dead letter and manual replay | Extended outage needs operator intervention | Receiver-outage E2E; claim/send/ack fault tests at attempts 1 and 10; concurrent-worker lease test |
| Secret leakage in logs/artifacts | Env or secret store; redaction; no key in API; pinned CI dependencies | Host compromise | Static secret scan and log assertions |
| Worker overload or stuck call | Context deadlines, bounded worker/queue/backoff, readiness failure | Sustained outage causes lag | RPC/DB cancellation, bounded-listener shutdown, outbox cancellation and race tests |

## OWASP Smart Contract checklist

Access control: role tests and delayed admin. Oracle manipulation: no price oracle. Logic errors: state-machine and invariant tests. Input validation: addresses, amount, expiry, duplicate ID. Reentrancy and unchecked external calls: guard, effects-before-interactions, SafeERC20 with exact balance delta. Flash-loan/economic surface: no price-sensitive logic or credit. Arithmetic: uint128 bound, no float. Randomness: UUID only for uniqueness, never economic randomness. Denial of service: bounded token calls, no enumeration loops; token blacklist/revert can block a payout and is a documented residual risk. Actual tests and Slither results are recorded in `docs/evidence.md`.

## Fail-closed triggers

Unknown canonical ancestor within max depth, contradictory RPC block hash, corrupted checkpoint, missing required token/contract configuration, invalid webhook secret, or unavailable DB must fail readiness and stop affected work. Do not infer payment success from HTTP submission or pending transaction status.

## Release proof boundary

The release checker, unlike the watcher, fetches successful transaction receipts, checks canonical block hashes and at least six confirmations, verifies exact creation/funding/terminal topics, calldata, participants and amounts for two distinct anchored escrow IDs, and compares compiled runtime and verified-source hashes. The checked-in `docs/sepolia-evidence.json` anchors public historical identities; the ignored sidecar adds exact release HEAD/CI binding. A response that is truthful when checked can later be invalidated by a reorg, and a dishonest provider is not made trustworthy by internally consistent responses. Source verification uses the explorer as another trusted service.

There has been no professional third-party audit. Original webhook logs assert successful HMAC verification and agree with saved outbox/state history; the historical HMAC cannot be independently recomputed because the ephemeral secret/signatures were not retained. Public evidence must state this limitation.
