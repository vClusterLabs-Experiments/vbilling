BINARY := vbilling
IMAGE  := ghcr.io/loft-sh/vbilling
TAG    ?= latest

.PHONY: build run test docker-build docker-push clean tidy

build:
	CGO_ENABLED=0 go build -o bin/$(BINARY) ./cmd/vbilling

run: build
	./bin/$(BINARY)

test:
	go test ./... -v -race

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
