# Security status

SettleKit is a testnet reference implementation, **not an independently audited production financial system**. It must not hold real assets. The existing MockUSDC and PaymentEscrow deployments and separate release/refund demonstrations are on Ethereum Sepolia only; see `docs/deployment-evidence.md` and `docs/sepolia-evidence.json`.

Reorg, outbox, release-evidence, and configuration defects were found and corrected. Regression evidence is in `docs/evidence.md`. The project has not had a professional third-party security audit.

See `docs/security-review.md` for accepted Slither findings and residual risks and `docs/threat-model.md` for trust boundaries. Report vulnerabilities privately to the repository maintainer using an agreed private channel; do not post secrets or exploit details in public issues.
