# srsran-operator Makefile
# Modelled after the free5gc-operator Makefile for consistency.

# Image and chart settings
IMG           ?= docker.io/nephio/srsran-operator:latest
CONTROLLER_GEN_VERSION ?= v0.12.0
ENVTEST_K8S_VERSION    ?= 1.27.x

# Tool binaries
LOCALBIN  ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST         ?= $(LOCALBIN)/setup-envtest

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests and RBAC role files.
	$(CONTROLLER_GEN) rbac:roleName=srsran-operator-role crd webhook paths="./..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: controller-gen ## Regenerate zz_generated.deepcopy.go.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run unit tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test ./... -coverprofile cover.out

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build operator binary.
	go build -o bin/srsran-operator ./main.go

.PHONY: run
run: manifests generate fmt vet ## Run operator against the current kubeconfig cluster.
	go run ./main.go

.PHONY: docker-build
docker-build: ## Build Docker image.
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push Docker image.
	docker push $(IMG)

##@ Deployment

.PHONY: install
install: manifests ## Install CRDs into the cluster.
	kubectl apply -f config/crd/bases

.PHONY: uninstall
uninstall: manifests ## Remove CRDs from the cluster.
	kubectl delete --ignore-not-found=true -f config/crd/bases

.PHONY: deploy
deploy: manifests ## Deploy controller to the cluster.
	kubectl apply -k config/default

.PHONY: undeploy
undeploy: ## Undeploy controller from the cluster.
	kubectl delete --ignore-not-found=true -k config/default

##@ Build Dependencies

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

$(LOCALBIN):
	mkdir -p $(LOCALBIN)
