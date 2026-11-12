# Part I build. Requires: Go >= 1.26, protoc (https://protobuf.dev/installation/).
MODULE := github.com/your-team/inventory
CORS ?=           # e.g. make run CORS='*'  (only for the API-tool containers on :8090-:8092)
export PATH := $(shell go env GOPATH)/bin:$(PATH)

.PHONY: setup gen gen-rest gen-proto build run run2 demo check tools-up tools-down swagger-up swagger-down clean

# One-time: record the code generators as Go tools (pinned in go.mod) and install them.
setup:
	@command -v protoc >/dev/null || { echo "protoc not found. Install it: https://protobuf.dev/installation/ (macOS: brew install protobuf)"; exit 1; }
	go get -tool github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@latest
	go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go get -tool google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	go install tool

gen: gen-rest gen-proto

# Step 2: OpenAPI -> Go HTTP server (gen/rest/rest.gen.go)
gen-rest:
	mkdir -p gen/rest
	go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi.yaml

# Steps 3-4: .proto -> Go messages + gRPC stubs (gen/inventory/v1/*.pb.go)
gen-proto:
	@command -v protoc >/dev/null || { echo "protoc not found. Install it: https://protobuf.dev/installation/ (macOS: brew install protobuf)"; exit 1; }
	protoc -I proto \
	  --go_out=. --go_opt=module=$(MODULE) \
	  --go-grpc_out=. --go-grpc_opt=module=$(MODULE) \
	  proto/inventory/v1/inventory.proto

build: gen
	go mod tidy
	go vet ./...
	go build -o bin/server ./cmd/server

run: build
	./bin/server -id n1 -http :8080 -grpc :50051 -cors-origin '$(CORS)'

# Second node whose REST API forwards every call to n1 over gRPC.
run2:
	./bin/server -id n2 -http :8081 -grpc :50052 -target localhost:50051

demo:
	sh ./etc/demo.sh

# Installs the generators, builds, starts two nodes and checks every status code.
check:
	sh ./etc/check.sh

# Optional: self-hosted Swagger Editor (:8090), Swagger UI (:8091), Scalar (:8092).
TOOLS := docker compose -f docker/api-tools.compose.yaml
tools-up:
	$(TOOLS) up -d
	@echo "swagger editor: http://localhost:8090  swagger ui: http://localhost:8091  scalar: http://localhost:8092"

tools-down:
	$(TOOLS) down

swagger-up: tools-up      # old names
swagger-down: tools-down

clean:
	rm -rf gen bin
