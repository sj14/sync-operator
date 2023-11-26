export GOBIN=$(PWD)/bin

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	bin/controller-gen crd webhook paths="./..." output:crd:artifacts:config=deploy/crds/

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	bin/controller-gen object paths="./..."

.PHONY: test
test: manifests generate envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell bin/setup-envtest use --bin-dir $(PWD)/bin/ -p path)" go test -race ./... -coverprofile cover.out

##@ Build

.PHONY: build
build: manifests generate ## Build manager binary.
	go build -o bin/manager main.go

.PHONY: run
run: manifests generate ## Run a controller from your host.
	go run ./main.go


##@ Deployment

.PHONY: install
install: manifests  ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	kubectl apply -Rf deploy/crds

.PHONY: uninstall
uninstall: manifests ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	kubectl delete --ignore-not-found=true -Rf deploy/crds

.PHONY: deploy
deploy: manifests ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	kubectl apply -Rf deploy/

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	kubectl delete --ignore-not-found=true -Rf deploy/

##@ Build Dependencies

## Tool Binaries
.PHONY: controller-gen
controller-gen:
	cd tools && go install -tags tools sigs.k8s.io/controller-tools/cmd/controller-gen

.PHONY: envtest
envtest:
	cd tools && go install -tags tools sigs.k8s.io/controller-runtime/tools/setup-envtest
