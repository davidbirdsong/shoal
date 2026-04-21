# Personal overrides (AWS profile, ECR URL, etc.) go in Makefile.local.
# Copy Makefile.local.example to Makefile.local and fill in your values.
# Makefile.local is gitignored.
-include Makefile.local

IMAGE      ?= shoal
AWS_REGION ?= us-west-2

# ECR_REPO and AWS_PROFILE must be set in Makefile.local for push/login targets.
ECR_REPO   ?=
AWS_PROFILE ?=

GIT_SHA   := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
GIT_DIRTY := $(shell git status --porcelain 2>/dev/null | head -1)
GIT_TAG   := $(GIT_SHA)$(if $(GIT_DIRTY),-dirty,)

# proto target requires: protoc, protoc-gen-go
# install: go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
PROTO_SRC := proto/shoal.proto
PROTO_OUT := pkg/proto

.PHONY: build push login clean tag proto compose-build test test-integration

tag:
	@echo $(GIT_TAG)

build:
	docker build --platform linux/arm64 -t $(IMAGE):latest .

proto:
	PATH="$(shell go env GOPATH)/bin:$$PATH" protoc \
		--go_out=$(PROTO_OUT) --go_opt=paths=source_relative \
		--proto_path=proto $(PROTO_SRC)

compose-build:
	docker compose build

test:
	go test ./...

test-integration:
	go test -tags integration -v ./cmd/

clean:
	docker rmi $(IMAGE):latest 2>/dev/null || true

# Targets below require ECR_REPO and AWS_PROFILE from Makefile.local.

check-push-vars:
	@test -n "$(ECR_REPO)"    || (echo "ECR_REPO is not set — copy Makefile.local.example to Makefile.local"; exit 1)
	@test -n "$(AWS_PROFILE)" || (echo "AWS_PROFILE is not set — copy Makefile.local.example to Makefile.local"; exit 1)

login: check-push-vars
	aws ecr get-login-password --region $(AWS_REGION) --profile $(AWS_PROFILE) | \
		docker login --username AWS --password-stdin $(ECR_REPO)

push: build login
	docker tag $(IMAGE):latest $(ECR_REPO):$(GIT_TAG)
	docker tag $(IMAGE):latest $(ECR_REPO):latest
	docker push $(ECR_REPO):$(GIT_TAG)
	docker push $(ECR_REPO):latest
	@echo "Pushed $(ECR_REPO):$(GIT_TAG)"
