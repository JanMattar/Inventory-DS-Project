#!/usr/bin/env sh
# End-to-end check of the generators and the running servers.
#   ./etc/check.sh        (from the project root; needs Go >= 1.26, protoc, curl)
# 1. installs the generators, 2. generates + vets + builds, 3. starts two nodes,
# 4. calls every operation and compares the HTTP status code with the spec.
set -eu
cd "$(dirname "$0")/.."

step() { printf '\n== %s\n' "$*"; }

step "1/4 generators"
command -v protoc >/dev/null || { echo "protoc not found: https://protobuf.dev/installation/"; exit 1; }
make setup >/dev/null
go tool oapi-codegen -version 2>/dev/null | head -1 || true
protoc --version

step "2/4 generate, vet, build"
make build

step "3/4 start n1 (:18080/:15051) and n2 (:18081 -> n1)"
./bin/server -id n1 -http :18080 -grpc :15051 >/tmp/inventory-n1.log 2>&1 & P1=$!
./bin/server -id n2 -http :18081 -grpc :15052 -target localhost:15051 >/tmp/inventory-n2.log 2>&1 & P2=$!
trap 'kill $P1 $P2 2>/dev/null' EXIT
for i in 1 2 3 4 5 6 7 8 9 10; do curl -s -o /dev/null localhost:18080/openapi.yaml && break; sleep 0.3; done

step "4/4 every operation"
FAIL=0
J='Content-Type: application/json'
# expect <code> <method> <path> [curl args...]
expect() {
  want=$1 m=$2 p=$3; shift 3
  got=$(curl -s -o /tmp/inventory-body -w '%{http_code}' -X "$m" "$B$p" "$@")
  if [ "$got" = "$want" ]; then printf '  ok   %s %-6s %s\n' "$got" "$m" "$p"
  else printf '  FAIL %s %-6s %s (want %s): %s\n' "$got" "$m" "$p" "$want" "$(head -c 200 /tmp/inventory-body)"; FAIL=1; fi
}
for B in http://localhost:18080 http://localhost:18081; do
  echo " $B"
  # reads
  expect 200 GET    "/v1/products"
  expect 200 GET    "/v1/products?sku=apple&sku=kiwi&sort=-stock"
  expect 200 GET    "/v1/products?prefix=b&inStock=true&limit=1&consistency=linearizable"
  expect 400 GET    "/v1/products?sku=apple&prefix=a"
  expect 400 GET    "/v1/products?consistency=linearisable"
  expect 400 GET    "/v1/products?sort=color"
  expect 400 GET    "/v1/products?limit=notanumber"
  expect 200 GET    "/v1/products/apple?consistency=linearizable"
  expect 404 GET    "/v1/products/kiwi"
  # create
  expect 201 POST   "/v1/products" -H "$J" -d '{"sku":"kiwi","name":"Kiwi","priceCents":30,"stock":5}'
  expect 409 POST   "/v1/products" -H "$J" -d '{"sku":"apple","name":"Apple","priceCents":50}'
  expect 400 POST   "/v1/products" -H "$J" -d '{"sku":"","name":"Nothing","priceCents":1}'
  expect 400 POST   "/v1/products" -H "$J" -d '{bad json'
  # update
  expect 200 PATCH  "/v1/products/apple" -H "$J" -d '{"priceCents":60}'
  expect 200 PATCH  "/v1/products/apple" -H "$J" -H 'If-Match: "3"' -d '{"name":"Green apple"}'
  expect 412 PATCH  "/v1/products/apple" -H "$J" -H 'If-Match: "7"' -d '{"priceCents":1}'
  expect 400 PATCH  "/v1/products/apple" -H "$J" -H 'If-Match: abc' -d '{"priceCents":1}'
  expect 404 PATCH  "/v1/products/kiwi"  -H "$J" -d '{"priceCents":1}'
  expect 400 PATCH  "/v1/products/apple" -H "$J" -d '{}'
  # delete one / several
  expect 204 DELETE "/v1/products/apple"
  expect 412 DELETE "/v1/products/apple" -H 'If-Match: "9"'
  expect 404 DELETE "/v1/products/kiwi"
  expect 204 DELETE "/v1/products?sku=apple&sku=banana"
  expect 404 DELETE "/v1/products?sku=apple&sku=kiwi"
  expect 400 DELETE "/v1/products?sku=apple&sku=apple"
  expect 400 DELETE "/v1/products"
  # read-modify-write: stock and orders
  expect 200 PATCH  "/v1/products/cherry/stock" -H "$J" -d '{"add":5}'
  expect 400 PATCH  "/v1/products/cherry/stock" -H "$J" -d '{"add":0}'
  expect 404 PATCH  "/v1/products/kiwi/stock"   -H "$J" -d '{"add":5}'
  expect 200 POST   "/v1/orders" -H "$J" -d '{"items":[{"sku":"apple","quantity":2},{"sku":"banana","quantity":3}]}'
  expect 409 POST   "/v1/orders" -H "$J" -d '{"items":[{"sku":"cherry","quantity":1}]}'
  expect 404 POST   "/v1/orders" -H "$J" -d '{"items":[{"sku":"apple","quantity":1},{"sku":"kiwi","quantity":1}]}'
  expect 400 POST   "/v1/orders" -H "$J" -d '{"items":[{"sku":"apple","quantity":0}]}'
  expect 400 POST   "/v1/orders" -H "$J" -d '{"items":[{"sku":"apple","quantity":1},{"sku":"apple","quantity":1}]}'
  # safe retries (the dummy pretends every client's last write was number 5)
  expect 200 POST   "/v1/orders" -H "$J" -H 'Client-Id: alice' -H 'Request-Seq: 6' -d '{"items":[{"sku":"apple","quantity":1}]}'
  expect 200 POST   "/v1/orders" -H "$J" -H 'Client-Id: alice' -H 'Request-Seq: 5' -d '{"items":[{"sku":"apple","quantity":1}]}'
  expect 409 POST   "/v1/orders" -H "$J" -H 'Client-Id: alice' -H 'Request-Seq: 4' -d '{"items":[{"sku":"apple","quantity":1}]}'
  expect 400 POST   "/v1/orders" -H "$J" -H 'Client-Id: alice' -d '{"items":[{"sku":"apple","quantity":1}]}'
done

# Details a status code does not show.
B=http://localhost:18080
curl -si "$B/v1/products/apple" | grep -qi '^etag: "3"' && echo "  ok   ETag header" || { echo "  FAIL ETag header"; FAIL=1; }
curl -si -X POST "$B/v1/products" -H "$J" -d '{"sku":"kiwi","name":"Kiwi","priceCents":30}' | grep -qi '^location: /v1/products/kiwi' && echo "  ok   Location header" || { echo "  FAIL Location header"; FAIL=1; }
curl -si "$B/v1/products/kiwi" | grep -qi '^content-type: application/problem+json' && echo "  ok   problem+json errors" || { echo "  FAIL problem+json errors"; FAIL=1; }
curl -si "$B/v1/products?limit=notanumber" | grep -qi '^content-type: application/problem+json' && echo "  ok   problem+json for bad parameters" || { echo "  FAIL problem+json for bad parameters"; FAIL=1; }
curl -s "$B/v1/products?inStock=true" | grep -q 'cherry' && { echo "  FAIL inStock filter"; FAIL=1; } || echo "  ok   inStock filter"
curl -s "http://localhost:18081/v1/products/apple" | grep -q 'from n1' && echo "  ok   n2 forwards to n1" || { echo "  FAIL n2 forwards to n1"; FAIL=1; }

echo
[ $FAIL = 0 ] && echo "ALL CHECKS PASSED" || { echo "SOME CHECKS FAILED (server logs: /tmp/inventory-n1.log, /tmp/inventory-n2.log)"; exit 1; }
