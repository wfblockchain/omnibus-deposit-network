SERVICE_NAME := payments-svc
OFAC_DIR ?= data/ofac

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

# ──────────────── Code Generation ────────────────

.PHONY: generate
generate: proto ent wire ## Run all code generation

.PHONY: proto
proto: ## Generate protobuf, gRPC, Kratos HTTP code and the OpenAPI spec
	protoc --proto_path=. \
		--proto_path=./third_party \
		--go_out=paths=source_relative:. \
		--go-grpc_out=paths=source_relative:. \
		--go-http_out=paths=source_relative:. \
		'--openapi_out=fq_schema_naming=true,default_response=false,title=Omnibus network payment hub API,version=1.0.0:internal/server' \
		api/payments/v1/*.proto

.PHONY: ent
ent: ## Generate Ent ORM code
	go generate ./ent

.PHONY: wire
wire: ## Generate Wire dependency injection code
	cd cmd/$(SERVICE_NAME) && go run github.com/google/wire/cmd/wire@v0.7.0

# ──────────────── Build & Run ────────────────

.PHONY: build
build: ## Build the binaries (production)
	go build -trimpath -ldflags="-w -s" -o bin/$(SERVICE_NAME) ./cmd/$(SERVICE_NAME)

.PHONY: build-demo
build-demo: ## Build the demo binaries (static tokens, scenario clock)
	go build -tags demo -trimpath -ldflags="-w -s" -o bin/$(SERVICE_NAME) ./cmd/$(SERVICE_NAME)
	go build -tags demo -trimpath -ldflags="-w -s" -o bin/payments-portal ../payments-portal/cmd/payments-portal

.PHONY: run
run: build-demo ## Build and run locally against anvil and SQLite
	./bin/$(SERVICE_NAME) -conf ./configs

.PHONY: ofac-data
ofac-data: ## Download the current OFAC SDN list (sdn, add, alt, comments)
	go run ./cmd/ofac-download -dir $(OFAC_DIR)

# ──────────────── Testing ────────────────

.PHONY: test
test: ## Unit tests
	go test -race -count=1 ./internal/...

.PHONY: e2e
e2e: ## End-to-end test over HTTP (needs anvil and a Foundry build)
	go test -count=1 -v ./test/...

# ──────────────── Docker ────────────────

.PHONY: docker-build
docker-build: ## Build the image (context: the repository root)
	docker build -t $(SERVICE_NAME):latest -f Dockerfile ../..

.PHONY: docker-up
docker-up: ## Start PostgreSQL for local runs
	docker run -d --name payments-pg -e POSTGRES_USER=payments -e POSTGRES_PASSWORD=payments \
		-e POSTGRES_DB=payments -p 55432:5432 postgres:16-alpine
