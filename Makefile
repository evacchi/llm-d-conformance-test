GO ?= go

# Manifest repo
MANIFEST_REPO ?= https://github.com/aneeshkp/llm-d-conformance-manifests.git
MANIFEST_DIR ?= deploy/manifests

# Default settings
PLATFORM ?= any
NAMESPACE ?= llm-conformance-test
KUBECONFIG ?= $(HOME)/.kube/config
REPORT_DIR ?= reports
TESTCASE_DIR ?= configs/testcases
PROFILE ?=
TESTCASE ?=
STORAGE_CLASS ?=
STORAGE_SIZE ?=
MODE ?= deploy
ENDPOINT ?=
MODEL_SOURCE ?= hf
MODEL ?=
NO_CLEANUP ?=
DISCOVER ?=
MOCK ?=
PULL_SECRET ?=

# Build flags
GO_TEST_FLAGS ?= -v
GINKGO_FLAGS ?= --ginkgo.v --ginkgo.fail-fast --ginkgo.silence-skips
TEST_FLAGS = -platform=$(PLATFORM) -namespace=$(NAMESPACE) -kubeconfig=$(KUBECONFIG) \
             -report-dir=$(REPORT_DIR) -testcase-dir=$(TESTCASE_DIR)

ifdef STORAGE_CLASS
  TEST_FLAGS += -storage-class=$(STORAGE_CLASS)
endif
ifdef STORAGE_SIZE
  TEST_FLAGS += -storage-size=$(STORAGE_SIZE)
endif
ifdef PROFILE
  TEST_FLAGS += -profile=$(PROFILE)
endif
ifdef TESTCASE
  TEST_FLAGS += -testcase=$(TESTCASE)
endif
ifneq ($(MODE),deploy)
  TEST_FLAGS += -mode=$(MODE)
endif
ifneq ($(MODEL_SOURCE),hf)
  TEST_FLAGS += -model-source=$(MODEL_SOURCE)
endif
ifdef ENDPOINT
  TEST_FLAGS += -endpoint=$(ENDPOINT)
endif
ifdef MODEL
  TEST_FLAGS += -model=$(MODEL)
endif
ifdef NO_CLEANUP
  TEST_FLAGS += -nocleanup
endif
ifdef DISCOVER
  TEST_FLAGS += -mode=discover
endif
ifdef MOCK
  TEST_FLAGS += -mock=$(MOCK)
endif
ifdef PULL_SECRET
  TEST_FLAGS += -pull-secret=$(PULL_SECRET)
endif

.PHONY: help
help: ## Show this help
	@printf '\n'
	@printf '\033[1mTargets:\033[0m\n'
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-25s\033[0m %s\n", $$1, $$2}'
	@printf '\n'
	@printf '\033[1mFlags:\033[0m\n'
	@printf '  \033[36m%-25s\033[0m %s\n' "TESTCASE" "Test case name"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make test TESTCASE=single-gpu"
	@printf '  \033[36m%-25s\033[0m %s\n' "MODEL" "Override default model"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make test TESTCASE=single-gpu MODEL=Qwen/Qwen2.5-7B-Instruct"
	@printf '  \033[36m%-25s\033[0m %s\n' "MODEL_SOURCE" "Model source: hf (default) or pvc (pre-cached)"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make test TESTCASE=single-gpu MODEL_SOURCE=pvc"
	@printf '  \033[36m%-25s\033[0m %s\n' "MANIFEST_REF" "Manifest repo branch (default: main)"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make setup MANIFEST_REF=3.4-ea1"
	@printf '  \033[36m%-25s\033[0m %s\n' "MANIFEST_REPO" "Manifest repo URL"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make setup MANIFEST_REPO=https://github.com/myorg/manifests.git"
	@printf '  \033[36m%-25s\033[0m %s\n' "NO_CLEANUP" "Keep resources after test for debugging"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make test TESTCASE=single-gpu NO_CLEANUP=1"
	@printf '  \033[36m%-25s\033[0m %s\n' "DISCOVER" "Validate existing deployment (skip deploy/cleanup)"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make test TESTCASE=single-gpu DISCOVER=true NAMESPACE=my-ns"
	@printf '  \033[36m%-25s\033[0m %s\n' "MOCK" "Mock vLLM image for testing without GPU"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make test TESTCASE=single-gpu MOCK=ghcr.io/aneeshkp/vllm-mock:latest"
	@printf '  \033[36m%-25s\033[0m %s\n' "PULL_SECRET" "Pull secret name to copy into namespace (default: auto-detect)"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make test TESTCASE=single-gpu PULL_SECRET=my-registry-secret"
	@printf '  \033[36m%-25s\033[0m %s\n' "STORAGE_CLASS" "StorageClass for PVCs (default: cluster default)"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make cache-model TESTCASE=single-gpu STORAGE_CLASS=azurefile-rwx"
	@printf '  \033[36m%-25s\033[0m %s\n' "STORAGE_SIZE" "Override PVC storage size (default: from test case config)"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make cache-model TESTCASE=single-gpu STORAGE_SIZE=50Gi"
	@printf '  \033[36m%-25s\033[0m %s\n' "NAMESPACE" "K8s namespace (default: llm-conformance-test)"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make test TESTCASE=single-gpu NAMESPACE=my-ns"
	@printf '  \033[36m%-25s\033[0m %s\n' "KUBECONFIG" "Path to kubeconfig (default: $$KUBECONFIG)"
	@printf '  \033[33m%-25s\033[0m %s\n' "" "make test TESTCASE=single-gpu KUBECONFIG=~/.kube/my-cluster"
	@printf '\n'

.PHONY: deps
deps: ## Install Go dependencies
	$(GO) mod tidy
	$(GO) mod download

.PHONY: build
build: deps ## Build and verify compilation
	$(GO) build ./...

# ─── Manifest Setup ─────────────────────────────────────────────

.PHONY: setup
setup: ## Clone manifest repo (usage: make setup or make setup MANIFEST_REF=3.4-ea1)
ifdef HELP
	@echo "Usage: make setup [MANIFEST_REF=<branch>] [MANIFEST_REPO=<url>]"
	@echo ""
	@echo "Clone or update the manifest repo into deploy/manifests/"
	@echo ""
	@echo "Flags:"
	@echo "  MANIFEST_REF   Branch or tag (default: main). Examples: 3.4-ea1, 3.4-ea2"
	@echo "  MANIFEST_REPO  Git repo URL (default: $(MANIFEST_REPO))"
	@echo ""
	@echo "Examples:"
	@echo "  make setup                          # clone main (latest)"
	@echo "  make setup MANIFEST_REF=3.4-ea1     # clone EA1 branch"
	@echo "  make setup MANIFEST_REF=3.4-ea2     # clone EA2 branch"
	@echo "  make delete-manifests               # remove cloned manifests"
else
ifdef MANIFEST_REF
	@echo "Cloning manifests (branch=$(MANIFEST_REF))..."
	@rm -rf $(MANIFEST_DIR)
	@git clone --depth 1 --branch $(MANIFEST_REF) $(MANIFEST_REPO) $(MANIFEST_DIR)
else
	@echo "Cloning manifests (branch=main)..."
	@rm -rf $(MANIFEST_DIR)
	@git clone --depth 1 $(MANIFEST_REPO) $(MANIFEST_DIR)
endif
	@cd $(MANIFEST_DIR) && git rev-parse --abbrev-ref HEAD > version.txt && git rev-parse --short HEAD >> version.txt
	@echo "Manifests ready at $(MANIFEST_DIR)/"
	@cat $(MANIFEST_DIR)/version.txt | paste -sd ' ' - | xargs -I{} echo "  version: {}"
	@ls $(MANIFEST_DIR)/*.yaml 2>/dev/null | wc -l | xargs -I{} echo "  {} manifest files"
endif

.PHONY: delete-manifests
delete-manifests: ## Remove cloned manifests
	@rm -rf $(MANIFEST_DIR)
	@echo "Manifests removed. Run 'make setup' to re-clone."

.PHONY: lint
lint: ## Run golangci-lint and go vet
	@which golangci-lint > /dev/null 2>&1 || { echo "golangci-lint not found. Install: https://golangci-lint.run/welcome/install/"; exit 1; }
	golangci-lint run ./...
	$(GO) vet ./...

# ─── Framework Validation ────────────────────────────────────────

.PHONY: test-smoke
test-smoke: # Run smoke tests (framework validation, no cluster needed)
	$(GO) test ./tests/smoke/ $(GO_TEST_FLAGS) -timeout 30m -args $(GINKGO_FLAGS)

# ─── Single Test Case ────────────────────────────────────────────

.PHONY: test
test: # Run a single test case
ifndef TESTCASE
	$(error TESTCASE is required. Usage: make test TESTCASE=single-gpu or make test TESTCASE=single-gpu,cache-aware)
endif
	$(MAKE) test-conformance TESTCASE=$(TESTCASE)

# ─── Conformance Suite ───────────────────────────────────────────

.PHONY: test-conformance
test-conformance: # (internal) Run conformance suite with a profile or flags
	$(GO) test ./tests/ $(GO_TEST_FLAGS) -timeout 6h -args $(GINKGO_FLAGS) $(TEST_FLAGS)

# ─── Test Profiles ───────────────────────────────────────────────

.PHONY: test-profile-smoke
test-profile-smoke: # Quick smoke test
	$(MAKE) test-conformance PROFILE=configs/profiles/smoke.yaml

.PHONY: test-profile-all
test-profile-all: # All conformance tests
	$(MAKE) test-conformance PROFILE=configs/profiles/all.yaml

.PHONY: test-profile-cache-aware
test-profile-cache-aware: # Cache-aware routing
	$(MAKE) test-conformance PROFILE=configs/profiles/cache-aware.yaml

.PHONY: test-profile-pd
test-profile-pd: # P/D disaggregation
	$(MAKE) test-conformance PROFILE=configs/profiles/pd.yaml

.PHONY: test-profile-moe
test-profile-moe: # MoE (8 GPUs + RDMA)
	$(MAKE) test-conformance PROFILE=configs/profiles/deepseek.yaml

# ─── Benchmark (helmfile-based) ──────────────────────────────────

HELMFILE ?=
GUIDES ?=
HELMFILE_ENV ?= default
JUNIT_REPORT ?=

BENCHMARK_FLAGS = --ginkgo.label-filter=benchmark

ifdef JUNIT_REPORT
  GINKGO_FLAGS += --ginkgo.junit-report=$(abspath $(JUNIT_REPORT))
endif

ifdef HELMFILE
  BENCHMARK_FLAGS += -helmfile=$(HELMFILE)
endif
ifdef GUIDES
  BENCHMARK_FLAGS += -guides=$(GUIDES)
endif
ifneq ($(HELMFILE_ENV),default)
  BENCHMARK_FLAGS += -helmfile-env=$(HELMFILE_ENV)
endif
ifdef BENCHMARK_IMAGE
  BENCHMARK_FLAGS += -benchmark-image=$(BENCHMARK_IMAGE)
endif
ifdef BENCHMARK_DATA
  BENCHMARK_FLAGS += -benchmark-data=$(BENCHMARK_DATA)
endif
ifdef BENCHMARK_RATE
  BENCHMARK_FLAGS += -benchmark-rate=$(BENCHMARK_RATE)
endif
ifdef BENCHMARK_MAX_SECONDS
  BENCHMARK_FLAGS += -benchmark-max-seconds=$(BENCHMARK_MAX_SECONDS)
endif
ifdef STREAM_LOGS
  BENCHMARK_FLAGS += -stream-logs
endif

.PHONY: test-benchmark
test-benchmark: ## Run benchmark smoke tests via helmfile
ifndef HELMFILE
	$(error HELMFILE is required. Usage: make test-benchmark HELMFILE=deploy/helmfiles/is-vanilla/helmfile.yaml.gotmpl GUIDES=/path/to/guides)
endif
	$(GO) test ./tests/ $(GO_TEST_FLAGS) -timeout 2h -args $(GINKGO_FLAGS) $(TEST_FLAGS) $(BENCHMARK_FLAGS)

# ─── Model Caching ───────────────────────────────────────────────

.PHONY: cache-models
cache-models: ## Pre-download all models to PVCs (run before tests to speed them up)
	$(MAKE) test-conformance MODE=cache

.PHONY: cache-model
cache-model: ## Pre-download a single model (usage: make cache-model TESTCASE=single-gpu)
ifndef TESTCASE
	$(error TESTCASE is required. Usage: make cache-model TESTCASE=single-gpu)
endif
	$(MAKE) test-conformance MODE=cache TESTCASE=$(TESTCASE)

# ─── Discover (validate existing deployment) ─────────────────────

# ─── Utilities ───────────────────────────────────────────────────

.PHONY: delete-testcase
delete-testcase: ## Delete a test case config (usage: make delete-testcase TESTCASE=my-test)
ifndef TESTCASE
	$(error TESTCASE is required. Usage: make delete-testcase TESTCASE=my-test)
endif
	@echo "Deleting test case: $(TESTCASE)"
	@f="configs/testcases/$(TESTCASE).yaml"; \
	if [ -f "$$f" ]; then echo "  rm $$f"; rm "$$f"; else echo "  $$f (not found)"; fi
	@echo ""
	@echo "NOTE: Also delete the manifest from the manifest repo if needed:"
	@echo "  cd <manifest-repo> && rm $(TESTCASE).yaml && git commit -am 'Remove $(TESTCASE)'"
	@echo "  Then re-run: make setup"
	@echo ""
	@echo "Remember to remove $(TESTCASE) from any profiles in configs/profiles/"

.PHONY: clean
clean: ## Remove generated reports
	rm -rf $(REPORT_DIR)/*.json

.PHONY: profiles
profiles: ## List available test profiles
	@echo ""
	@if [ -f "$(MANIFEST_DIR)/version.txt" ]; then \
		printf '  \033[1mManifests:\033[0m %s\n' "$$(cat $(MANIFEST_DIR)/version.txt | paste -sd ' ' -)"; \
	else \
		printf '  \033[33mManifests: not cloned — run make setup\033[0m\n'; \
	fi
	@echo ""
	@echo "Available profiles:"
	@echo ""
	@for f in configs/profiles/*.yaml; do \
		name=$$(grep '^name:' $$f | head -1 | awk '{print $$2}'); \
		desc=$$(grep '^description:' $$f | head -1 | sed 's/^description: *"//;s/"$$//'); \
		printf "  \033[1mmake test-profile-%-10s\033[0m %s\n" "$$name" "$$desc"; \
		grep -A100 '^testCases:' $$f | tail -n+2 | grep '  - ' | sed 's/  - //' | while read tc; do \
			printf "    \033[36m- %s\033[0m\n" "$$tc"; \
		done; \
		echo ""; \
	done
	@echo "Override flags:"
	@echo "  make test-profile-all MODEL=Qwen/Qwen2.5-7B-Instruct"
	@echo "  make test-profile-all MODEL_SOURCE=pvc"
	@echo "  make test-profile-smoke NO_CLEANUP=1"
	@echo ""
	@echo "Discover (validate existing deployment):"
	@echo "  make test-profile-all DISCOVER=true NAMESPACE=my-ns"

.PHONY: testcases
testcases: ## List available test cases grouped by category
	@echo ""
	@if [ -f "$(MANIFEST_DIR)/version.txt" ]; then \
		printf '  \033[1mManifests:\033[0m %s\n\n' "$$(cat $(MANIFEST_DIR)/version.txt | paste -sd ' ' -)"; \
	else \
		printf '  \033[33mManifests: not cloned — run make setup\033[0m\n\n'; \
	fi
	@for category in single-node-gpu cache-aware multi-node-gpu deepseek; do \
		case $$category in \
			single-node-gpu) header="Single-node GPU" ;; \
			multi-node-gpu)  header="Multi-node GPU (P/D)" ;; \
			cache-aware)     header="Cache-aware routing" ;; \
			deepseek)        header="DeepSeek MoE (8+ GPUs)" ;; \
		esac; \
		printf '  \033[1m%s:\033[0m\n' "$$header"; \
		for f in configs/testcases/*.yaml; do \
			cat=$$(grep '  category:' $$f | head -1 | awk '{print $$2}'); \
			if [ "$$cat" = "$$category" ]; then \
				name=$$(grep '^name:' $$f | head -1 | awk '{print $$2}'); \
				manifest=$$(grep 'manifestPath:' $$f | head -1 | awk '{print $$2}'); \
				desc=$$(grep '^description:' $$f | head -1 | sed 's/^description: *"//;s/"$$//'); \
				if [ -f "$(MANIFEST_DIR)/$$manifest" ]; then \
					printf "    \033[36m%-30s\033[0m %s\n" "$$name" "$$desc"; \
				else \
					printf "    \033[36m%-30s\033[0m %s \033[33m(manifest missing)\033[0m\n" "$$name" "$$desc"; \
				fi; \
			fi; \
		done; \
		echo ""; \
	done
	@echo "Run a test case:"
	@echo "  make test TESTCASE=single-gpu"
	@echo "  make test TESTCASE=single-gpu MODEL=Qwen/Qwen2.5-7B-Instruct"
	@echo "  make test TESTCASE=cache-aware NO_CLEANUP=1"
	@echo ""
	@echo "Discover (validate existing deployment):"
	@echo "  make test TESTCASE=single-gpu DISCOVER=true NAMESPACE=my-ns"
	@echo ""
	@echo "Mock mode (no GPU required):"
	@echo "  make test TESTCASE=single-gpu MOCK=ghcr.io/aneeshkp/vllm-mock:latest"
	@echo ""

.PHONY: models
models: ## List models and URIs for all test cases
	@echo ""
	@printf "  \033[36m%-30s %-45s %s\033[0m\n" "TEST CASE" "MODEL" "URI"
	@echo "  $(shell printf '%.0s─' {1..100})"
	@for f in configs/testcases/*.yaml; do \
		name=$$(grep '^name:' $$f | head -1 | awk '{print $$2}'); \
		model=$$(grep '  name:' $$f | head -1 | awk '{print $$2}'); \
		uri=$$(grep '  uri:' $$f | head -1 | awk '{print $$2}'); \
		printf "  %-30s %-45s %s\n" "$$name" "$$model" "$$uri"; \
	done
	@echo ""

