BINARY := vbilling
IMAGE  := ghcr.io/vclusterlabs-experiments/vbilling
TAG    ?= latest

.PHONY: build run test lint e2e docker-build docker-push clean tidy

build:
	CGO_ENABLED=0 go build -o bin/$(BINARY) ./cmd/vbilling

run: build
	./bin/$(BINARY)

test:
	go test ./... -race

lint:
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal; exit 1)
	go vet ./...

# Full end-to-end run on kind with real tenant clusters, stripe-mock and a
# signed-webhook receiver (needs docker, kind, vcluster, helm, jq).
e2e:
	./scripts/e2e-kind.sh

docker-build:
	docker build -t $(IMAGE):$(TAG) .

docker-push: docker-build
	docker push $(IMAGE):$(TAG)

tidy:
	go mod tidy

clean:
	rm -rf bin/

helm-install:
	helm upgrade --install vbilling deploy/helm/vbilling \
		--namespace vbilling-system --create-namespace

helm-uninstall:
	helm uninstall vbilling -n vbilling-system
