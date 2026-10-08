# Release verification

The public Sepolia record contains one mock token, one escrow contract, and two separate payment intents: one released and one refunded. The deployed token is freely mintable and has no real value. No transaction needs to be sent to verify these records.

## Local and CI gates

Run `make local-verify` before publishing a commit. It covers Go formatting, vet, unit/race/fuzz smoke, PostgreSQL and Anvil integration, Foundry unit/fuzz/invariant tests, Slither, gas snapshots, vulnerability scans, a clean clone, the container image, SPDX SBOM, and image provenance. GitHub Actions runs the same local gate for the exact pushed commit.

`make verify` adds a read-only live release check. It requires:

- a clean working tree and a successful GitHub Actions run for the current commit;
- `docs/release-evidence.json`, generated locally from the tracked `docs/sepolia-evidence.json`;
- `SETTLEKIT_RELEASE_GITHUB_REPOSITORY` set to the published repository in `owner/name` form;
- an HTTPS Sepolia RPC URL in `SETTLEKIT_TESTNET_RPC_URL` and an Etherscan API key in `ETHERSCAN_API_KEY`.

The checked-in evidence file contains no credentials or local paths. Its `github_repository` field records the source repository associated with the historical evidence; it is not the CI target for this standalone copy. The ignored sidecar binds the same chain evidence to the current commit and its published CI repository without creating a self-referential commit hash:

```sh
python3 - <<'PYCODE'
import json
import os
import pathlib
import subprocess

anchor = json.loads(pathlib.Path('docs/sepolia-evidence.json').read_text())
head = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
repository = os.environ['SETTLEKIT_RELEASE_GITHUB_REPOSITORY']
anchor['commit'] = head
anchor['ci_repository'] = repository
anchor['ci_checks_url'] = f"https://github.com/{repository}/commit/{head}/checks"
sidecar = pathlib.Path('docs/release-evidence.json')
sidecar.write_text(json.dumps(anchor, indent=2) + '\n')
sidecar.chmod(0o600)
PYCODE
```

Keep the RPC URL and API key in a local secret store or environment, never in a command argument that is logged, a repository file, or a CI artifact. The sidecar contains only public identifiers and is excluded from Git. Run `make verify` after the exact commit has a completed successful CI workflow in the selected repository. Until this copy is published and CI completes there, the strict gate remains pending; `make local-verify` is the reproducible local gate.

## What the live check proves

The checker requires Sepolia chain ID `11155111`, at least six canonical confirmations, successful deployment/funding/terminal receipts, and exact transaction destinations, senders, calldata, events, participants, amounts, deadlines, and distinct escrow IDs. It rechecks block hashes and confirmation depth at the end of the run. It builds the contracts with `forge build --force --no-dynamic-test-linking --silent`, compares the full deployed runtime (including the immutable token address), compiler version, and source hashes against live code and Etherscan's verified source, and requires CI success for the current commit. The receipt checks use read-only RPC methods.

The observed Sepolia payments were made with an earlier backend revision. The current backend is exercised by PostgreSQL and Anvil integration tests; this check does not turn historical transactions into a new end-to-end Sepolia run. A chain reorganization or dishonest provider can invalidate later observations. The controlled receiver recorded successful HMAC verification during the historical scenarios, but the ephemeral secret and signatures were not retained for independent recomputation.

If any live result differs, stop the release and investigate the source, provider, block, or evidence file. Do not lower confirmation thresholds or change expected transaction identities to make the gate pass. No production deployment or real-asset use is authorized by a passing check.
