# Reproducible local demo

Run from the repository root:

```sh
make anvil-test
```

Requires Go 1.27.1, PostgreSQL 18 server binaries, Foundry 1.8.4, Python 3, `jq`, `curl`, and `openssl`. `SETTLEKIT_PG_BIN` can point at PostgreSQL's binary directory. When `SETTLEKIT_TEST_DATABASE_URL` is supplied, the script uses that isolated test database instead of starting a temporary cluster. The test destroys only its own temporary cluster, Anvil process, proxy, and receiver.

The script performs these assertions with local ephemeral accounts:

1. Deploy mock USDC and `PaymentEscrow`, mint payer funds, and approve the escrow.
2. Create each intent through the authenticated API and check the Go escrow ID against the contract's `escrowIdFor` result.
3. Fund from the payer wallet and observe `OBSERVED`; send SIGTERM and restart the service before confirmation for the first intent.
4. Mine the threshold block and observe `CONFIRMED`.
5. Request unsigned release/refund through operator-authenticated endpoints, submit from the local operator wallet, and observe terminal status and exact payer/payee balances.
6. Revert an Anvil snapshot after a confirmed release, observe `PAYMENT_REVERSED`, then replay a release on the replacement branch.
7. Stop the webhook receiver before a refund, restart the service and receiver, then verify HMAC-signed terminal and reversal notifications. The receiver rejects an invalid signature or stale timestamp.
8. Stop the primary JSON-RPC proxy, mine another block, and verify that the watcher advances the checkpoint and readiness stays healthy through the configured fallback.

The final output is `Anvil create/fund/confirm/release/refund, reorg reversal, restart recovery, RPC failover, signed webhooks and Go/contract escrow ID: PASS`. Intermediate service logs and transaction hashes live only in the temporary fixture directory, which is removed after the run; no external network deployment is implied.

`make db-test` runs fresh PostgreSQL schema/API/indexer/outbox tests normally and under Go's race detector. `make local-verify` adds formatting, vet, Go fuzz smoke, Foundry, Slither, image build, Trivy, SPDX SBOM and local SLSA provenance checks. `make verify` additionally requires real testnet and CI release evidence.


Additional deterministic PostgreSQL regressions cover A -> B -> A for released and refunded payments, loss of confirmations with the event block retained, interrupted reconciliation transactions, immutable log-set checks, the affected-payment bound, and outbox claim/send/ack failures at attempts 1 and 10. These fault-injection tests model persisted crash boundaries; the Anvil script itself uses graceful process termination. `make release-checker-test` runs offline adversarial evidence checks; strict `make verify` always uses live Sepolia/GitHub/Etherscan and unchanged deployment sources.
