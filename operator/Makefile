CONTROLLER_GEN ?= controller-gen
CHART_DIR := ../deploy/helm/kubecost-auto-hpa

.PHONY: generate manifests test test-all race integration envtest e2e-kind fmt vet build check
generate:
	$(CONTROLLER_GEN) object paths=./api/...

manifests: generate
	$(CONTROLLER_GEN) crd rbac:roleName=kubecost-auto-hpa-manager paths=./... \
		output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac
	cp config/crd/bases/*.yaml $(CHART_DIR)/crds/

fmt:
	gofmt -w .
vet:
	go vet ./...
build:
	go build -o bin/manager ./cmd
test:
	go test ./...

# Data-race detector (memory hungry: limit package parallelism).
race:
	go test -race -p 2 ./...

# Everything that needs no cluster: format, vet, tests, race, integration (real Python engine).
check: fmt vet test race integration
# End-to-end flow against the real Python engine (needs python3).
integration:
	go test -tags integration -count=1 ./...

# Against a real kube-apiserver + etcd. KUBEBUILDER_ASSETS must point to a directory holding
# the kube-apiserver and etcd binaries (see the envtest job in .github/workflows/ci.yml).
# Fails fast: without KUBEBUILDER_ASSETS the envtest tests would skip and the run would look green.
envtest:
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS must point to kube-apiserver and etcd"; exit 1; }
	go test -tags "envtest integration" -count=1 ./internal/controller/...

# Complete operator suite: unit tests plus every real-apiserver test, under the race detector.
test-all:
	@test -n "$$KUBEBUILDER_ASSETS" || { echo "KUBEBUILDER_ASSETS must point to kube-apiserver and etcd"; exit 1; }
	go test -tags "envtest integration" -race -p 2 -count=1 ./...

# Real cluster via kind (needs docker, kind, kubectl).
e2e-kind:
	cd .. && hack/e2e-kind.sh
