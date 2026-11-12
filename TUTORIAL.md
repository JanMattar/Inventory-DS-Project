# Tutorial: from the template to your own service

This tutorial has two parts. **Part A** (about 20 minutes) runs the template and follows one request through every layer. **Part B** replaces the key-value example with your own service, one step of the assignment at a time. Each step ends with a ✅ checkpoint you can verify on your own.

```
 client ──REST/JSON──▶ HTTP handlers ──gRPC──▶ gRPC service
         api/openapi.yaml   internal/httpapi   proto/…/*.proto   internal/grpcsvc
         (Steps 1–2)        (Step 5)           (Step 3)          (Step 4)
```

You edit four files: `api/openapi.yaml`, the `.proto` file, `internal/grpcsvc/server.go` and `internal/httpapi/handlers.go`. Everything under `gen/` is generated. Never edit it by hand.

---

## Part A: Get to know the template

### A1. Install and run

Prerequisites: Go ≥ 1.26, and `protoc`, the protobuf compiler: install it following https://protobuf.dev/installation/ (on macOS: `brew install protobuf`). `make setup` cannot install it for you, because it is not a Go tool. Optional: `grpcurl` and Docker.

```sh
make setup        # once, needs internet: installs the code generators, pinned in go.mod
make run          # generates code, builds, starts node n1: REST :8080, gRPC :50051
```

In a second terminal:

```sh
./etc/demo.sh     # calls every operation and prints the status codes
```

✅ You see `200`, `201`, `204`, `400`, `404` and `412` responses, and values like `dummy from n1`.

Or run `make check`: it does all of the above (and starts a second node) and prints `ALL CHECKS PASSED` if every operation returns the status code the spec documents.

### A2. Look at the API in the browser

Open **http://localhost:8080/docs** (Swagger UI). Each row is one operation in `api/openapi.yaml`. Open `GET /v1/keys/{key}`, click **Try it out**, enter `alpha`, then click **Execute**. Note the `ETag: "1"` response header. Now try the key `missing` to get a 404 with an `application/problem+json` body.

Open **http://localhost:8080/editor** (Swagger Editor). It loads the same file. Here you edit; the right pane shows the result live.

### A3. Follow one request through the code

Take `POST /v1/keys/alpha` with the body `{"value":"42"}`:

| # | Where | What happens |
|---|---|---|
| 1 | `api/openapi.yaml`, `paths./v1/keys/{key}.post` | The operation is declared with `operationId: setKey`, an optional `If-Match` header, and the responses 200, 201, 400 and 412. |
| 2 | `gen/rest/rest.gen.go` (generated) | `oapi-codegen` turned it into a method `SetKey(ctx, SetKeyRequestObject)` on `StrictServerInterface`. It also generated the types `SetKey200JSONResponse` and `SetKey201JSONResponse`. |
| 3 | `internal/httpapi/handlers.go`, `SetKey` | The handler converts `If-Match` to a version. It opens an `Update` stream, sends one `UpdateRequest{op: set}` and closes the stream. |
| 4 | `internal/grpcsvc/server.go`, `Update` | The server reads the whole stream (`recvAll`) and checks the versions. It answers with the entries and, for each, whether the key was created. |
| 5 | back in `SetKey` | `created` decides between 201 and 200. A gRPC error goes to `httpStatusFromGRPC` instead, e.g. `FAILED_PRECONDITION` → 412. |

✅ Try `curl -si -X POST localhost:8080/v1/keys/alpha -H 'Content-Type: application/json' -H 'If-Match: "7"' -d '{"value":"x"}'` and explain why it returns 412.

### A4. The three kinds of RPC streams

The example uses one RPC per kind of operation, and streams carry a variable number of keys:

| RPC | Stream | Handler side | Server side |
|---|---|---|---|
| `Read` | server → client | `stream.Recv()` until `io.EOF` (`recvEntries`) | `stream.Send()` once per entry |
| `Update` | client → server | `Send()` per key, then `CloseAndRecv()` (`h.update`) | `Recv()` until `io.EOF`, then `SendAndClose()` once |
| `Delete` | client → server | same as `Update` (`h.delete`) | same as `Update` |

`GET /v1/keys/{key}` and `GET /v1/keys` both use `Read`. `POST` and `PATCH` both use `Update`, with the operation `set` for POST and `append` or `increment` for PATCH. So eight REST operations share three RPCs. That's allowed: what matters is that every REST operation can be served by the RPCs, and that you write the mapping down.

### A5. Two nodes

```sh
make run2                               # node n2: REST :8081 → gRPC of n1
./etc/demo.sh http://localhost:8081     # values say "from n1"
```

✅ n2 answers REST calls by issuing RPCs to n1. This is the forwarding required in *Client Communication*.

---

## Part B: Build your own service

Pick your service first (not a key-value store). Write down in a few lines: the objects it manages, the operations, which operations only read and which change state, and at least one read-modify-write operation. In the bank-account example used below, `transfer` is the read-modify-write operation.

> **Tip:** Work on one operation through all five steps first, end to end, before adding the rest. Until an RPC is implemented it answers `UNIMPLEMENTED` (HTTP 501), so the project keeps compiling.

### Step 1: Write `api/openapi.yaml`

Open http://localhost:8080/editor (or https://editor.swagger.io). Replace the key-value `paths` and `schemas` with yours. Keep the `Consistency` parameter, the `Problem` response and the overall layout. For example:

```yaml
paths:
  /v1/accounts/{id}:
    parameters:
      - { name: id, in: path, required: true, schema: { type: string } }
    get:
      operationId: getAccount
      parameters:
        - $ref: '#/components/parameters/Consistency'
      responses:
        '200':
          description: The account
          content:
            application/json:
              schema: { $ref: '#/components/schemas/Account' }
        '404': { $ref: '#/components/responses/Problem' }
```

Design checklist:
- Use plural nouns for resources (`/v1/accounts`).
- Use HTTP methods for actions: `GET` reads, `POST` creates or sets, `PATCH` modifies, `DELETE` removes.
- Document every status code an operation can return.
- Pass options as query parameters.

The editor cannot save to your disk: **File → Save as YAML**, then overwrite `api/openapi.yaml`.

✅ The editor shows no errors or warnings.

### Step 2: Generate the HTTP server

```sh
make gen-rest     # api/openapi.yaml → gen/rest/rest.gen.go
go build ./...
```

The build now **fails** in `handlers.go` at `var _ rest.StrictServerInterface = (*Handlers)(nil)`. This is expected: the handlers still implement the old operations. Open `gen/rest/rest.gen.go` and find `type StrictServerInterface interface`. It lists one method per `operationId`, which you implement in Step 5. For now, delete the key-value handler methods. Then let your editor generate stubs that return `nil, status.Error(codes.Unimplemented, "todo")`.

✅ `go build ./...` succeeds.

### Step 3: Write the `.proto` file

Rename `proto/kvs/v1/kvs.proto` to fit your service, e.g. `proto/bank/v1/bank.proto` with `package bank.v1;` and `option go_package = "github.com/your-team/kvs/gen/bank/v1;bankv1";`. Update the path in the `gen-proto` target of the `Makefile`.

Rules (see the example):
1. **Every RPC has its own request message and its own response message**, even when two look alike.
2. **Use streams** when a call carries a variable number of items.
3. **Report errors with gRPC status codes**, not with error fields.
4. **RPCs need not map 1:1 to REST operations.** Write down which RPCs serve each REST operation (a table in your report).

```proto
service Bank {
  rpc GetAccounts(GetAccountsRequest) returns (stream GetAccountsResponse);
  rpc Transfer(TransferRequest) returns (TransferResponse);
}
```

✅ `make gen-proto` succeeds and `gen/bank/v1/` contains `bank.pb.go` and `bank_grpc.pb.go`.

### Step 4: Implement the gRPC service with dummy responses

In `internal/grpcsvc/server.go`:
1. Change the import to your generated package.
2. Embed `bankv1.UnimplementedBankServer` in `Server`.
3. Implement each RPC with a **dummy** response that includes `s.NodeID`.
4. Return realistic errors for fixed inputs, e.g. `status.Errorf(codes.NotFound, …)` for the id `missing`. This lets you test every error path end to end.

Register the service in `cmd/server/main.go` with `bankv1.RegisterBankServer(gs, grpcsvc.New(*id))`. Pass the client `bankv1.NewBankClient(conn)` to `httpapi.NewHTTPHandler`.

✅ With `make run` running: `grpcurl -plaintext localhost:50051 list` shows your service, and calling an RPC returns a dummy reply.

### Step 5: Implement the HTTP handlers

In `internal/httpapi/handlers.go`, implement every method of `StrictServerInterface`:
1. Convert the request object (path parameters, `Params`, `Body`) into the request message(s).
2. Call the RPC(s). For streams, reuse the pattern of `recvEntries` or `h.update`.
3. Convert the reply into the generated response type, e.g. `rest.GetAccount200JSONResponse{…}`.
4. On error, just `return nil, err`. Map gRPC codes to HTTP codes **only** in `httpStatusFromGRPC`, and keep it consistent with the responses in `openapi.yaml`.

Handlers must not keep application state: no maps and no counters.

Finally, rewrite `etc/demo.sh` with one `curl` per operation and per documented error.

✅ `./etc/demo.sh` shows the documented status codes, both on `:8080` and through `make run2` on `:8081`.

---

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `undefined: rest.XxxRequestObject` | The spec changed but the code was not regenerated. Run `make gen`. |
| Build fails at `var _ rest.StrictServerInterface` | A handler is missing or its signature is outdated. Compare it with `gen/rest/rest.gen.go`. |
| `make: protoc: No such file or directory` | `protoc` is not a Go tool, so `make setup` cannot install it. macOS: `brew install protobuf`; Debian/Ubuntu: `sudo apt install protobuf-compiler`; others: https://protobuf.dev/installation/. |
| `protoc-gen-go: program not found` | Run `make setup`; the Makefile puts `$(go env GOPATH)/bin` on `PATH`. |
| Every call returns 501 | The RPC is not implemented yet (it falls through to `Unimplemented…Server`). |
| A handler returns 500 for an expected error | `httpStatusFromGRPC` has no case for that gRPC code. |
| "Try it out" fails from the Docker tools on `:8090`–`:8092` | The call is cross-origin. Start the server with `make run CORS='*'` and select the `http://localhost:8080` server. |
| `ETag` is not visible in browser tools | Only when cross-origin: CORS mode exposes it via `Access-Control-Expose-Headers`. |
