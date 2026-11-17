.PHONY: test test-rust test-e2e test-e2e-container fuzz

test:
	bash scripts/test.sh

test-rust:
	bash scripts/test-rust.sh

test-e2e:
	bash scripts/test-e2e.sh

test-e2e-container:
	bash scripts/test-e2e.sh --container $(if $(BASE_IMAGE),--base-image "$(BASE_IMAGE)",)

fuzz:
	"$(or $(GO),go)" test ./kvp -run '^$$' -fuzz FuzzProvisioningReport -fuzztime=30s -parallel=2
	"$(or $(GO),go)" test ./kvp -run '^$$' -fuzz FuzzDecodeEntries -fuzztime=30s -parallel=2
