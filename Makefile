DOCKER_USERNAME ?= k8svol
LATEST_TAG ?= ci
IMAGE_TAG ?= $(shell git rev-parse --short HEAD)

.PHONY: vendor
vendor:
	@GO111MODULE=on go mod tidy
	@GO111MODULE=on go mod vendor

.PHONY: rsync-source-bin
rsync-source-bin: vendor
	@mkdir -p bin
	@rm -rf bin/rsync-source
	@CGO_ENABLED=0 go build -o bin/rsync-source app/rsync-source/*

.PHONY: rsync-source-image
rsync-source-image:
	docker build -t ghcr.io/$(DOCKER_USERNAME)/rsync-source:$(LATEST_TAG) -f package/Dockerfile.rsync .
	docker build -t ghcr.io/$(DOCKER_USERNAME)/rsync-source:$(IMAGE_TAG) -f package/Dockerfile.rsync .

.PHONY: push-rsync-source-image
push-rsync-source-image: rsync-source-image
	docker push ghcr.io/$(DOCKER_USERNAME)/rsync-source:$(LATEST_TAG)
	docker push ghcr.io/$(DOCKER_USERNAME)/rsync-source:$(IMAGE_TAG)

.PHONY: volume-source-bin
volume-source-bin: vendor
	@mkdir -p bin
	@rm -rf bin/volume-source
	@CGO_ENABLED=0 go build -o bin/volume-source app/volume-source/*

.PHONY: volume-source-image
volume-source-image:
	docker build -t ghcr.io/$(DOCKER_USERNAME)/volume-source:$(LATEST_TAG) -f package/Dockerfile.volume .
	docker build -t ghcr.io/$(DOCKER_USERNAME)/volume-source:$(IMAGE_TAG) -f package/Dockerfile.volume .

.PHONY: push-volume-source-image
push-volume-source-image: volume-source-image
	docker push ghcr.io/$(DOCKER_USERNAME)/volume-source:$(LATEST_TAG)
	docker push ghcr.io/$(DOCKER_USERNAME)/volume-source:$(IMAGE_TAG)

.PHONY: images
images: rsync-source-image volume-source-image

.PHONY: push-images
push-images: push-rsync-source-image push-volume-source-image
