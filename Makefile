.PHONY: build demo run-setup run-serve validate test test-integration test-public clean docker-build lint helm-lint helm-template helm-template-sidecar dev-compose-up dev-compose-down dev-compose-logs test-all

BINARY := medvfs
CMD_PATH := ./cmd/medvfs
EXAMPLE_CONFIG ?= deployments/examples/vfs-dev.yaml
DEMO_CONFIG := deployments/examples/demo/vfs-demo.yaml

build:
	go build -o $(BINARY) $(CMD_PATH)

# Self-contained 1-command demo (zero external services required)
demo: build
	rm -rf /tmp/medvfs-demo-out
	./$(BINARY) validate --config $(DEMO_CONFIG)
	./$(BINARY) setup --config $(DEMO_CONFIG)
	@echo "--- Demo output ---"
	@ls -la /tmp/medvfs-demo-out/
	@cat /tmp/medvfs-demo-out/manifest.json

# Run setup with an example spec (requires a running OMOP Postgres)
run-setup: build
	./$(BINARY) setup --config $(EXAMPLE_CONFIG)

# Validate a spec without fetching data
validate: build
	./$(BINARY) validate --config $(EXAMPLE_CONFIG)

# Unit + integration tests (integration requires OMOP_DSN env var)
test:
	go test ./internal/... -v -timeout 60s

test-integration:
	go test ./internal/... -v -tags integration -timeout 120s

# Live smoke tests against public FHIR R4, Orthanc DICOMweb, and HTTP Range servers
test-public:
	go test -tags public ./test/public/... -v -timeout 60s

# Start a local OMOP test database via Docker (for development)
dev-db:
	docker run -d --name omop-dev \
	  -e POSTGRES_USER=omop_user \
	  -e POSTGRES_PASSWORD=omop_pass \
	  -e POSTGRES_DB=omop_cdm \
	  -p 5432:5432 \
	  postgres:16-alpine

dev-db-stop:
	docker stop omop-dev && docker rm omop-dev

clean:
	rm -f $(BINARY)

# Docker image build
docker-build:
	docker build -t colabbio/medvfs:latest .

# Run serve mode (FUSE daemon — requires root/CAP_SYS_ADMIN)
run-serve: build
	sudo ./$(BINARY) serve --config $(EXAMPLE_CONFIG)

# Check formatting and linting
lint:
	gofmt -l ./...
	go vet ./...

# Helm chart operations
helm-lint:
	helm lint deployments/helm/medvfs/

helm-template:
	helm template medvfs deployments/helm/medvfs/ --debug

helm-template-sidecar:
	helm template medvfs deployments/helm/medvfs/ --set mode=sidecar --debug

# Local development environment (MedVFS + MinIO + dummy trainer)
dev-compose-up:
	docker compose -f deployments/examples/docker-compose.yaml up -d
	@echo "\n═══════════════════════════════════════"
	@echo " MedVFS dev environment running"
	@echo " MinIO Console: http://localhost:9001"
	@echo " MedVFS API:    http://localhost:8090"
	@echo "═══════════════════════════════════════"

dev-compose-down:
	docker compose -f deployments/examples/docker-compose.yaml down -v

dev-compose-logs:
	docker compose -f deployments/examples/docker-compose.yaml logs -f medvfs

# Run all tests (unit + integration)
test-all:
	go test ./... -count=1 -timeout 120s

