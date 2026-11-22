# joule-budget: energy budgets for Capsule tenants.

ENVTEST_K8S_VERSION ?= 1.31.0
LOCALBIN            ?= $(CURDIR)/bin
CONTROLLER_GEN      ?= $(LOCALBIN)/controller-gen
ENVTEST             ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT       ?= $(LOCALBIN)/golangci-lint

CONTROLLER_GEN_VERSION ?= v0.20.0
ENVTEST_VERSION        ?= release-0.25
GOLANGCI_LINT_VERSION  ?= v2.13.2

.PHONY: all generate manifests test lint build run
all: generate manifests lint test build

generate: $(CONTROLLER_GEN) ## DeepCopy methods of the API types
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

manifests: $(CONTROLLER_GEN) ## CRD and RBAC from the kubebuilder markers
	$(CONTROLLER_GEN) crd rbac:roleName=joule-budget paths=./... output:crd:dir=config/crd output:rbac:dir=config/rbac

test: $(ENVTEST) ## Unit tests and envtest (a real kube-apiserver + etcd)
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./... -coverprofile cover.out

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(LOCALBIN)/joule-budget ./cmd

run: ## Run against the current kubeconfig (PROMETHEUS_URL defaults to a local port-forward)
	go run ./cmd --prometheus-url=$${PROMETHEUS_URL:-http://127.0.0.1:18428} --metrics-bind-address=:18080 --health-probe-bind-address=:18081

$(CONTROLLER_GEN):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
$(ENVTEST):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
$(GOLANGCI_LINT):
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
