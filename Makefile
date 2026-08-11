IMAGE       ?= interstellar
TAG         ?= latest
PLATFORMS   ?= linux/amd64,linux/arm64
DEV_DATA    ?= .devdata
MAX_IMAGE_MB ?= 30
# Multi-platform needs the docker-container driver; the default docker driver
# cannot do it. Kept as a dedicated builder so your default one is untouched.
BUILDER     ?= interstellar-builder

.DEFAULT_GOAL := help

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

## dev: run locally against ./.devdata (no Docker)
dev:
	@mkdir -p $(DEV_DATA)
	DATA_DIR=$(DEV_DATA) go run ./cmd/interstellar

## build: compile the binary for this machine
build:
	go build -trimpath -o bin/interstellar ./cmd/interstellar

## test: run the offline test suite
test:
	go test ./...

## lint: vet and formatting check
lint:
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

## docker: build the image for this machine and load it
docker:
	docker build -t $(IMAGE):$(TAG) .

## docker-multiarch: verify both target architectures build
docker-multiarch:
	@docker buildx inspect $(BUILDER) >/dev/null 2>&1 || \
		docker buildx create --name $(BUILDER) --driver docker-container
	docker buildx build --builder $(BUILDER) --platform $(PLATFORMS) -t $(IMAGE):$(TAG) .

## size: check the image against the <30MB target from §3
size: docker
	@bytes=$$(docker image inspect $(IMAGE):$(TAG) --format '{{.Size}}'); \
	mb=$$((bytes / 1000000)); \
	echo "image size: $${mb}MB (target < $(MAX_IMAGE_MB)MB)"; \
	if [ $$mb -gt $(MAX_IMAGE_MB) ]; then echo "OVER TARGET"; exit 1; fi

## run: run the built image on a named volume
run: docker
	docker run --rm -p 8422:8422 -v interstellar-data:/data \
		-e TZ=$${TZ:-Asia/Bangkok} \
		$(IMAGE):$(TAG)

## clean: remove build output and local dev data
clean:
	rm -rf bin $(DEV_DATA)

.PHONY: help dev build test lint docker docker-multiarch size run clean
