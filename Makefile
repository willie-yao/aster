.PHONY: all build build-server build-worker serve dev-actions image fixer-image test test-v e2e lint fmt tidy helm-check \
       fetch-data fetch-data-quick fetch-data-ai fetch-data-ai-quick \
       fe-install dev fe-build fe-check fe-lint \
       dist dist-ai clean clean-cache clean-all help

# Path to a consumer project directory containing project.yaml + prompts/system.md.
# Override on the command line, e.g.:
#   make fetch-data PROJECT_DIR=../capz-prow-ai-dashboard
PROJECT_DIR ?= configs/example

# Container image coordinates for `make image`.
IMAGE ?= ghcr.io/willie-yao/prow-ai-dashboard
VERSION ?= dev

# Default target
all: build

## ─── Go Backend ───────────────────────────────────────────────

# Build the data fetcher binary
build:
	cd backend && go build -o ../bin/fetcher ./cmd/fetcher/

# Build the Kubernetes-native API server binary
build-server:
	cd backend && go build -o ../bin/server ./cmd/server/

# Build the continuous watch worker binary
build-worker:
	cd backend && go build -o ../bin/worker ./cmd/worker/

# Serve fetched data + capability descriptor over HTTP (server-mode preview).
# Point a built SPA at it with -static-dir=frontend/dist for a self-contained run.
serve: build-server
	./bin/server -data-dir=frontend/public/data

# Server-mode preview WITH admin actions (File issue / Propose fix / Mark
# resolved). Builds the SPA and serves it from the API server so the capability
# descriptor advertises actions and the buttons render. AUTH_MODE=dev
# authenticates every request as an admin, so no OAuth or proxy setup is needed
# (dev only; never expose this server). Set BOT_TOKEN to a GitHub token for the
# writes to actually reach GitHub. Unlike `make dev` (Vite + HMR), this serves a
# static build, so rebuild to pick up frontend changes. Override PROJECT_DIR to
# resolve issue/fix repos.
dev-actions: build-server fe-build
	AUTH_MODE=dev BOT_TOKEN=$${BOT_TOKEN:-dev-token} ./bin/server \
		-data-dir=frontend/public/data \
		-static-dir=frontend/dist \
		-project-dir=$(PROJECT_DIR)

# Build the container image (fetcher + server + SPA). Override IMAGE/VERSION:
#   make image IMAGE=ghcr.io/you/prow-ai-dashboard VERSION=v1.2.3
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

# Build the drop-in engine image with git for fix generation.
fixer-image:
	docker build --target fixer-runtime --build-arg VERSION=$(VERSION) -t $(IMAGE)/fixer:$(VERSION) .

# Run all Go tests
test:
	cd backend && go test ./... -count=1

# Run Go tests with verbose output
test-v:
	cd backend && go test ./... -count=1 -v

# Run the end-to-end pipeline tests (hermetic: local fixtures + scripted model)
e2e:
	cd backend && go test ./internal/e2e/... -count=1 -v

# Run Go linter (requires golangci-lint)
lint:
	cd backend && golangci-lint run ./...

# Format Go code
fmt:
	cd backend && gofmt -w .

# Tidy Go modules
tidy:
	cd backend && go mod tidy

# Lint and render the Helm chart, including the owned Orka resources.
helm-check:
	bash deploy/helm/prow-ai-dashboard/test-render.sh

## ─── Data Fetching ────────────────────────────────────────────

# Fetch fresh test data from GCS into frontend/public/data/
fetch-data: build
	./bin/fetcher -project-dir=$(PROJECT_DIR) -builds=8 -workers=5 -out=frontend/public/data -timeout=5m

# Fetch minimal data (3 builds per job, faster)
fetch-data-quick: build
	./bin/fetcher -project-dir=$(PROJECT_DIR) -builds=3 -workers=5 -out=frontend/public/data -timeout=3m

# Fetch data with AI analysis (requires AI_TOKEN env var)
fetch-data-ai: build
	./bin/fetcher -project-dir=$(PROJECT_DIR) -builds=8 -workers=5 -out=frontend/public/data -timeout=30m -ai

# Fetch minimal data with AI analysis
fetch-data-ai-quick: build
	./bin/fetcher -project-dir=$(PROJECT_DIR) -builds=3 -workers=5 -out=frontend/public/data -timeout=5m -ai

## ─── Frontend ─────────────────────────────────────────────────

# Install frontend dependencies
fe-install:
	cd frontend && npm ci

# Start the Vite dev server
dev: fe-install
	cd frontend && npm run dev

# Build the frontend for production
fe-build: fe-install
	cd frontend && npm run build

# TypeScript type check. Use `tsc -b` (build mode): the root tsconfig.json is a
# solution file (files: [], project references), so `tsc --noEmit` against it
# checks nothing. `tsc -b` walks the referenced app/node projects.
fe-check:
	cd frontend && npx tsc -b

# Lint frontend sources
fe-lint: fe-install
	cd frontend && npm run lint

## ─── Full Pipeline ────────────────────────────────────────────

# Build everything: Go binary + fetch data + frontend
dist: fetch-data fe-build

# Build everything with AI analysis
dist-ai: fetch-data-ai fe-build

# Clean build artifacts and generated data
clean:
	rm -rf bin/ frontend/dist frontend/public/data/dashboard.json frontend/public/data/jobs/ frontend/public/data/flakiness.json frontend/public/data/orka_analysis.json

# Clean AI analysis cache (forces re-analysis on next fetch)
clean-cache:
	rm -f frontend/public/data/ai_cache.json

# Clean everything including cache
clean-all: clean clean-cache

## ─── Help ─────────────────────────────────────────────────────

help:
	@echo "prow-ai-dashboard — Make Targets"
	@echo ""
	@echo "  build              Build Go data fetcher binary"
	@echo "  build-server       Build Go API server binary"
	@echo "  serve              Serve fetched data + capabilities over HTTP"
	@echo "  dev-actions        Serve SPA + API with admin actions enabled (local auth)"
	@echo "  test               Run Go tests"
	@echo "  test-v             Run Go tests (verbose)"
	@echo "  lint               Run golangci-lint"
	@echo "  fmt                Format Go code"
	@echo "  tidy               Tidy Go modules"
	@echo "  helm-check         Lint and validate Helm chart renders"
	@echo ""
	@echo "  fetch-data         Fetch data from GCS (8 builds/job)"
	@echo "  fetch-data-quick   Fetch minimal data (3 builds/job)"
	@echo "  fetch-data-ai      Fetch data + AI analysis (needs AI_TOKEN)"
	@echo "  fetch-data-ai-quick  Fetch minimal data + AI analysis"
	@echo ""
	@echo "    Override PROJECT_DIR to point at a consumer repo, e.g.:"
	@echo "      make fetch-data PROJECT_DIR=../capz-prow-ai-dashboard"
	@echo "    Default: configs/example (renders an empty dashboard, smoke-test only)"
	@echo ""
	@echo "  fe-install         Install frontend npm dependencies"
	@echo "  dev                Start Vite dev server"
	@echo "  fe-build           Production build of frontend"
	@echo "  fe-check           TypeScript type check"
	@echo "  fe-lint            Lint frontend sources"
	@echo ""
	@echo "  dist               Full pipeline: build + fetch + frontend"
	@echo "  dist-ai            Full pipeline with AI analysis"
	@echo "  image              Build the container image (fetcher + server + SPA)"
	@echo "  fixer-image        Build the git-capable drop-in Orka fix image"
	@echo "  clean              Remove build artifacts and data"
	@echo "  clean-cache        Clear AI analysis cache"
	@echo "  clean-all          Clean everything including cache"
