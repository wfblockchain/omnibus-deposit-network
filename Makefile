# Clearing-house settlement model — build/test helpers.
.PHONY: help test gotest economics slither coverage all

help:
	@echo "  make test        forge test (contracts, via Docker)"
	@echo "  make gotest      go test ./... (optimiser + simulation)"
	@echo "  make economics   print the efficiency / accrual tables"
	@echo "  make slither     static analysis of the omnibus contracts (pip install slither-analyzer)"
	@echo "  make coverage    forge coverage of the omnibus contracts (lower bound: --ir-minimum)"
	@echo "  make all         everything above"

test:
	cd contracts && docker run --rm -v "$$PWD":/w -w /w ghcr.io/foundry-rs/foundry:stable "forge test"

gotest:
	go test ./... -count=1

economics:
	go run ./cmd/clearing-operator

slither:
	cd contracts && slither .

coverage:
	cd contracts && forge coverage --ir-minimum --match-path 'test/omnibus/*' --no-match-contract Invariant --report summary

all: gotest test economics
