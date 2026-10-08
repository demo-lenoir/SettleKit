# Verification record

SettleKit is tested as a local service and against two historical Sepolia escrow transactions. A passing test is evidence for the named behavior, not a security audit or a production-readiness claim. The current backend has not been rerun through the historical Sepolia scenarios.

| Property | Executable evidence |
|---|---|
| Exact funding, fixed token, expiry, roles, pause, terminal exclusivity | `make contract-verify`: Foundry unit/fuzz tests, 256-run stateful invariant (16,384 calls), gas snapshot, and reviewed Slither findings. |
| Durable idempotency and one operator action per intent | `internal/api/server_test.go` with PostgreSQL, including concurrent create and opposing release/refund requests. |
| Canonical status and reorg recovery | `internal/indexer/*_test.go` covers duplicate logs, A → B → A, lost confirmations, immutable block/log mismatch, affected-set bounds, and transaction rollback. |
| Signed delivery and crash recovery | `internal/webhook/*_test.go` covers HMAC inputs, retries, final-attempt lease recovery, concurrent claims, dead letters, and same-ID replay. |
| Local end-to-end path | `make anvil-test` exercises funding, confirmations, release, refund, restart, reorg, RPC failover, and webhook verification. |
| Clean checkout | `make clean-clone-test` clones the current tree and runs the Anvil scenario with the included Solidity dependencies. |
| Source and image checks | `make supply-chain-verify` runs Trivy, checks the non-root image, SPDX SBOM, and BuildKit provenance. `make go-check` runs formatting, vet, unit/race/fuzz smoke, and govulncheck. |
| Sepolia contract and payment records | `docs/sepolia-evidence.json` anchors two distinct intents. `make verify` additionally checks live canonical receipts, exact terms/events/calldata, runtime bytecode, verified source, and CI for the current commit. |
| Amount and address validation | Contract invalid-input and amount/deadline fuzz tests; API validation tests. |
| Fee or reverting token | Foundry adversarial token tests require atomic funding failure. |
| Reentrant token call | Foundry funding and payout reentrancy tests. |
| Admin, operator, and pauser separation | Foundry unauthorized-call, delayed admin transfer, and pause-policy tests. |
| Expired refund recipient | Foundry expiry-boundary and permissionless refund tests. |
| Immutable terminal state | Foundry release/refund exclusivity tests and stateful invariant. |
| Matching intent and escrow identity | Go/contract escrow-ID comparison in `make anvil-test`; changed-payer/terms contract test. |
| Exact integer amount storage | PostgreSQL migration constraints and schema integration tests. |
| Forward-only schema migration | Repeated migration application in `internal/store/schema_test.go`. |
| Configured confirmation policy | Startup policy tests in `cmd/settlekit/config_test.go`. |
| Unfunded expiry after indexed head | Payment state tests and reorg expiry recovery tests. |
| RPC cancellation and secret redaction | Indexer source tests and API logging assertions. |
| Bounded HTTP admission | Listener capacity/shutdown tests and health endpoint rate-limit tests. |
| Canonical checkpoint atomicity | PostgreSQL indexer transaction and injected-failure tests. |
| Webhook lease ownership | Concurrent-worker and canceled-acknowledgement recovery tests. |
| Metrics and readiness | `internal/telemetry/metrics_test.go` and Anvil readiness/failover checks. |
| Operator wallet boundary | API tests assert unsigned calldata and HTTP `202`; no server signing path. |
| Release source identity | Live `make verify` compares compiler, Etherscan source hashes, and runtime bytecode. |

## Reproduce the local checks

Run `make local-verify` on this snapshot to check Go tests and race/fuzz smoke, Foundry contracts and gas snapshots, PostgreSQL and Anvil integration, clean-clone execution, vulnerability scans, container behavior, SBOM and provenance. `make verify` additionally needs live Sepolia, Etherscan and successful CI for this exact public commit; that external gate remains pending until publication.

## Historical Sepolia observations

The mock token and escrow were deployed on Sepolia on 2026-10-02 UTC. Their source was verified on Etherscan. The token and escrow deployment transaction hashes are in `docs/sepolia-evidence.json`; [deployment evidence](deployment-evidence.md) records addresses, receipts, roles, and gas.

The release scenario funded escrow `0xf08e129ed42653680d5e063b1026acae56d87cb32bfbd2b37a3c01c5ee4d2a70` and paid its recorded payee 1,000,000 mock base units. The refund scenario used a separate escrow, `0x766752a5fa598e06043e14ba356d8371d4121bf116bcaf69081960193152c734`, and returned 1,000,000 base units to its payer. Funding and terminal transactions in both scenarios reached six canonical confirmations when recorded. Five webhook deliveries per scenario were logged as HMAC-verified by the controlled receiver. The original private evidence bundles and SHA-256 manifests remain outside Git; their historical HMAC results cannot be recomputed because the ephemeral signing material was not retained.

The strict checker re-reads chain state when run. A later reorganization or dishonest provider can invalidate a result that was true when observed. Current backend recovery and delivery behavior is demonstrated by PostgreSQL and Anvil integration tests, not by these historical transactions.
