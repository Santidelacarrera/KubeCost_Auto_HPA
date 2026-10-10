CONTROLLER_GEN ?= controller-gen
CHART_DIR := ../deploy/helm/kubecost-auto-hpa

.PHONY: generate manifests test integration fmt vet build
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
