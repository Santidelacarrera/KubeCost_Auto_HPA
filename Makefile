# Root Makefile for KubeCost Auto-HPA

.PHONY: all test test-engine test-operator lint helm-lint build roi load-test stripe-catalog help

all: test lint

help:
	@echo "Available targets:"
	@echo "  make test          - Run all unit and integration tests (Go operator + Python engine)"
	@echo "  make test-engine   - Run Python unit tests with pytest"
	@echo "  make test-operator - Run Go operator unit tests"
	@echo "  make lint          - Check code formatting, vet Go code, and lint Helm chart"
	@echo "  make helm-lint     - Run helm lint on deploy/helm/kubecost-auto-hpa"
	@echo "  make build         - Build the operator binary"
	@echo "  make roi           - Run the FinOps ROI calculator CLI"
	@echo "  make load-test     - Run the 500-concurrent-user load and stability simulation"
	@echo "  make stripe-catalog - Generate Stripe billing products and prices in dry-run mode"

test: test-engine test-operator

test-engine:
	python -m pytest engine/tests -q

test-operator:
	$(MAKE) -C operator test

lint: helm-lint
	$(MAKE) -C operator vet

helm-lint:
	helm lint deploy/helm/kubecost-auto-hpa

build:
	$(MAKE) -C operator build

roi:
	python scripts/calculate_roi.py --nodes 100 --avg-cpu-util 0.20

load-test:
	python scripts/simulate_500_concurrent_users.py --users 500 --requests 4

stripe-catalog:
	python scripts/setup_stripe_billing.py --dry-run --output web/stripe-config.json
