CONTROLLER_GEN ?= controller-gen
CHART_DIR := ../deploy/helm/kubecost-auto-hpa

.PHONY: generate manifests test integration envtest e2e-kind fmt vet build
generate:
	$(CONTROLLER_GEN) object paths=./api/...

manifests: generate
	$(CONTROLLER_GEN) crd rbac:roleName=kubecost-auto-hpa-manager paths=./... \
		output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac
	cp config/crd/bases/autoscaling.kubecost.io_kubecosthpas.yaml $(CHART_DIR)/crds/

fmt:
	gofmt -w .
vet:
	go vet ./...
build:
	go build -o bin/manager ./cmd
test:
	go test ./...
# End-to-end flow against the real Python engine (needs python3).
integration:
	go test -tags integration -count=1 ./...

# Against a real kube-apiserver + etcd. KUBEBUILDER_ASSETS must point to a directory holding
# the kube-apiserver and etcd binaries (see the envtest job in .github/workflows/ci.yml).
envtest:
	go test -tags "envtest integration" -count=1 ./internal/controller/...

# Real cluster via kind (needs docker, kind, kubectl).
e2e-kind:
	cd .. && hack/e2e-kind.sh
