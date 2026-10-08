.PHONY: docs-verify verify local-verify go-check test contract-test contract-verify db-test anvil-test clean-clone-test supply-chain-verify release-checker-test
GOVULNCHECK ?= $(shell go env GOPATH)/bin/govulncheck

docs-verify:
	python3 scripts/check_docs.py

contract-test:
	forge test

contract-verify:
	forge fmt --check
	forge test
	forge snapshot --check --match-test 'testCreateAndReleaseExactlyOnce|testEarlyRefundAndTerminalExclusivity|testExpiryBoundaryAndPermissionlessRefund' --snap contracts/gas-snapshot
	python3 scripts/check_slither.py

test:
	go test ./...

go-check:
	@unformatted="$$(gofmt -l $$(rg --files -g '*.go' -g '!contracts/lib/**'))"; test -z "$$unformatted" || { echo "unformatted Go files: $$unformatted" >&2; exit 1; }
	go vet ./...
	go test ./... -count=1
	go test -race ./... -count=1
	go test ./internal/payments -run '^$$' -fuzz '^FuzzTerminalCannotCrossToOtherTerminal$$' -fuzztime=3s -parallel=2
	$(GOVULNCHECK) ./...

db-test:
	bash scripts/test_postgres.sh

anvil-test:
	bash scripts/test_postgres.sh --anvil

clean-clone-test:
	bash scripts/test_clean_clone.sh

supply-chain-verify:
	bash scripts/verify_supply_chain.sh

release-checker-test:
	python3 -m unittest discover -s scripts -p 'test_release_evidence.py' -v

local-verify: release-checker-test docs-verify go-check contract-verify db-test anvil-test clean-clone-test supply-chain-verify

verify: local-verify
	python3 scripts/check_release_evidence.py
