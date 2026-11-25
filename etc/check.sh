#!/usr/bin/env sh
# End-to-end check of the generators and the running servers, on real state.
#   ./etc/check.sh        (from the project root; needs Go >= 1.26, protoc, curl)
# 1. installs the generators, 2. generates + vets + builds + runs the unit
# tests, 3. starts two nodes (n2 forwards to n1, so both share n1's state),
# 4. calls every operation through each node and compares the status codes
# with the spec, and checks that answers depend on the earlier writes.
set -eu
cd "$(dirname "$0")/.."

step() { printf '\n== %s\n' "$*"; }

step "1/4 generators"
command -v protoc >/dev/null || { echo "protoc not found: https://protobuf.dev/installation/"; exit 1; }
make setup >/dev/null
go tool oapi-codegen -version 2>/dev/null | head -1 || true
protoc --version

step "2/4 generate, vet, build, unit tests"
make build
go test -race ./...

step "3/4 start n1 (:18080/:15051, sample inventory) and n2 (:18081 -> n1)"
./bin/server -id n1 -http :18080 -grpc :15051 -snapshot etc/sample-snapshot.txtpb >/tmp/inventory-n1.log 2>&1 & P1=$!
./bin/server -id n2 -http :18081 -grpc :15052 -target localhost:15051 >/tmp/inventory-n2.log 2>&1 & P2=$!
trap 'kill $P1 $P2 2>/dev/null' EXIT
for i in 1 2 3 4 5 6 7 8 9 10; do curl -s -o /dev/null localhost:18080/openapi.yaml && break; sleep 0.3; done

step "4/4 every operation, on real state"
FAIL=0
J='Content-Type: application/json'
ok()   { printf '  ok   %s\n' "$*"; }
bad()  { printf '  FAIL %s\n' "$*"; FAIL=1; }
# expect <code> <method> <path> [curl args...]
expect() {
  want=$1 m=$2 p=$3; shift 3
  got=$(curl -s -o /tmp/inventory-body -w '%{http_code}' -X "$m" "$B$p" "$@")
  if [ "$got" = "$want" ]; then printf '  ok   %s %-6s %s\n' "$got" "$m" "$p"
  else bad "$got $m $p (want $want): $(head -c 200 /tmp/inventory-body)"; fi
}
# has <what> <path> <pattern>: the GET answer contains the pattern
has() { if curl -s "$B$2" | grep -q -- "$3"; then ok "$1"; else bad "$1: $3 not in $(curl -s "$B$2" | head -c 200)"; fi; }
# etag <sku>: the product's current ETag, e.g. "12"
etag() { curl -si "$B/v1/products/$1" | tr -d '\r' | awk 'tolower($1)=="etag:" {print $2}'; }
order() { printf '{"items":[%s]}' "$1"; }
batch() { printf '{"delete":[%s]}' "$1"; }

# Each node gets its own SKUs (prefix p1- or p2-) and its own client id, so
# both passes expect the same answers although they share n1's state.
N=0
for B in http://localhost:18080 http://localhost:18081; do
  N=$((N + 1)); P="p$N-"; C="client$N"
  echo " $B  (SKUs $P*)"
  # create
  expect 201 POST   "/v1/products" -H "$J" -d '{"skuId":"'$P'apple","name":"Apple","priceCents":50,"stock":10}'
  expect 201 POST   "/v1/products" -H "$J" -d '{"skuId":"'$P'banana","name":"Banana","priceCents":25,"stock":20}'
  expect 201 POST   "/v1/products" -H "$J" -d '{"skuId":"'$P'cherry","name":"Cherry","priceCents":400,"stock":0}'
  expect 409 POST   "/v1/products" -H "$J" -d '{"skuId":"'$P'apple","name":"Apple","priceCents":50}'
  expect 400 POST   "/v1/products" -H "$J" -d '{"skuId":"","name":"Nothing","priceCents":1}'
  expect 400 POST   "/v1/products" -H "$J" -d '{bad json'
  # reads
  expect 200 GET    "/v1/products?prefix=$P"
  expect 200 GET    "/v1/products?skuId=${P}apple&skuId=${P}kiwi&sort=-stock"
  expect 200 GET    "/v1/products?prefix=$P&inStock=true&limit=1&consistency=linearizable"
  expect 400 GET    "/v1/products?skuId=${P}apple&prefix=$P"
  expect 400 GET    "/v1/products?consistency=linearisable"
  expect 400 GET    "/v1/products?sort=color"
  expect 400 GET    "/v1/products?limit=notanumber"
  expect 200 GET    "/v1/products/${P}apple?consistency=linearizable"
  expect 404 GET    "/v1/products/${P}kiwi"
  has "a created product is read back" "/v1/products/${P}banana" '"stock":20'
  # update: the first write changes the version, so the old ETag is stale
  OLD=$(etag "${P}apple")
  expect 200 PATCH  "/v1/products/${P}apple" -H "$J" -d '{"priceCents":60}'
  expect 412 PATCH  "/v1/products/${P}apple" -H "$J" -H "If-Match: $OLD" -d '{"priceCents":1}'
  expect 200 PATCH  "/v1/products/${P}apple" -H "$J" -H "If-Match: $(etag "${P}apple")" -d '{"name":"Green apple"}'
  expect 400 PATCH  "/v1/products/${P}apple" -H "$J" -H 'If-Match: abc' -d '{"priceCents":1}'
  expect 404 PATCH  "/v1/products/${P}kiwi"  -H "$J" -d '{"priceCents":1}'
  expect 400 PATCH  "/v1/products/${P}apple" -H "$J" -d '{}'
  has "an update is seen by later reads" "/v1/products/${P}apple" '"name":"Green apple","priceCents":60'
  # read-modify-write: stock and orders
  expect 200 PATCH  "/v1/products/${P}cherry/stock" -H "$J" -d '{"add":5}'
  expect 400 PATCH  "/v1/products/${P}cherry/stock" -H "$J" -d '{"add":0}'
  expect 404 PATCH  "/v1/products/${P}kiwi/stock"   -H "$J" -d '{"add":5}'
  expect 200 POST   "/v1/orders" -H "$J" -d "$(order '{"skuId":"'$P'apple","quantity":2},{"skuId":"'$P'banana","quantity":3}')"
  expect 409 POST   "/v1/orders" -H "$J" -d "$(order '{"skuId":"'$P'apple","quantity":1},{"skuId":"'$P'cherry","quantity":6}')"
  expect 404 POST   "/v1/orders" -H "$J" -d "$(order '{"skuId":"'$P'apple","quantity":1},{"skuId":"'$P'kiwi","quantity":1}')"
  expect 400 POST   "/v1/orders" -H "$J" -d "$(order '{"skuId":"'$P'apple","quantity":0}')"
  expect 400 POST   "/v1/orders" -H "$J" -d "$(order '{"skuId":"'$P'apple","quantity":1},{"skuId":"'$P'apple","quantity":1}')"
  has "stock added, then sold: 0 + 5"  "/v1/products/${P}cherry" '"stock":5'
  has "a failed order changes nothing" "/v1/products/${P}apple"  '"stock":8'
  # safe retries: the same Request-Seq is not applied twice; an older one is refused
  expect 200 POST   "/v1/orders" -H "$J" -H "Client-Id: $C" -H 'Request-Seq: 1' -d "$(order '{"skuId":"'$P'apple","quantity":1}')"
  expect 200 POST   "/v1/orders" -H "$J" -H "Client-Id: $C" -H 'Request-Seq: 1' -d "$(order '{"skuId":"'$P'apple","quantity":1}')"
  has "a retry is not applied again" "/v1/products/${P}apple" '"stock":7'
  expect 200 POST   "/v1/orders" -H "$J" -H "Client-Id: $C" -H 'Request-Seq: 2' -d "$(order '{"skuId":"'$P'apple","quantity":1}')"
  expect 409 POST   "/v1/orders" -H "$J" -H "Client-Id: $C" -H 'Request-Seq: 1' -d "$(order '{"skuId":"'$P'apple","quantity":1}')"
  expect 400 POST   "/v1/orders" -H "$J" -H "Client-Id: $C" -d "$(order '{"skuId":"'$P'apple","quantity":1}')"
  has "new seq applied, stale seq not" "/v1/products/${P}apple" '"stock":6'
  # delete one (DELETE) / several (PATCH batch)
  expect 412 DELETE "/v1/products/${P}banana" -H 'If-Match: "1"'
  expect 204 DELETE "/v1/products/${P}banana"
  expect 404 DELETE "/v1/products/${P}banana"
  expect 404 GET    "/v1/products/${P}banana"
  expect 404 PATCH  "/v1/products" -H "$J" -d "$(batch '"'$P'apple","'$P'banana"')"
  expect 200 GET    "/v1/products/${P}apple"
  expect 400 PATCH  "/v1/products" -H "$J" -d "$(batch '"'$P'apple","'$P'apple"')"
  expect 400 PATCH  "/v1/products" -H "$J" -d "$(batch '')"
  expect 204 PATCH  "/v1/products" -H "$J" -d "$(batch '"'$P'apple","'$P'cherry"')"
  expect 404 GET    "/v1/products/${P}apple"
  has "the deleted products are gone" "/v1/products?prefix=$P" '"products":\[\]'
done

echo " concurrent writes: 100 x add 1, through n1 and n2 at once"
B=http://localhost:18080
expect 201 POST "/v1/products" -H "$J" -d '{"skuId":"pen","name":"Pen","priceCents":100}'
(
  for i in $(seq 50); do
    curl -s -o /dev/null -X PATCH "http://localhost:18080/v1/products/pen/stock" -H "$J" -d '{"add":1}' &
    curl -s -o /dev/null -X PATCH "http://localhost:18081/v1/products/pen/stock" -H "$J" -d '{"add":1}' &
  done
  wait
)
has "no update was lost: stock 100" "/v1/products/pen" '"stock":100'

# Details a status code does not show.
curl -si "$B/v1/products/apple" | grep -qi '^etag: "3"' && ok "ETag header (sample apple, version 3)" || bad "ETag header"
curl -si -X POST "$B/v1/products" -H "$J" -d '{"skuId":"kiwi","name":"Kiwi","priceCents":30}' | grep -qi '^location: /v1/products/kiwi' && ok "Location header" || bad "Location header"
curl -si "$B/v1/products/nope" | grep -qi '^content-type: application/problem+json' && ok "problem+json errors" || bad "problem+json errors"
curl -si "$B/v1/products?limit=notanumber" | grep -qi '^content-type: application/problem+json' && ok "problem+json for bad parameters" || bad "problem+json for bad parameters"
curl -s "$B/v1/products?inStock=true" | grep -q '"skuId":"cherry"' && bad "inStock filter" || ok "inStock filter"
# n2 starts empty: it can only know "apple" (and kiwi, just created through n1) by forwarding to n1
curl -s "http://localhost:18081/v1/products/kiwi" | grep -q '"skuId":"kiwi"' && ok "n2 forwards to n1 (shared state)" || bad "n2 forwards to n1"

echo
[ $FAIL = 0 ] && echo "ALL CHECKS PASSED" || { echo "SOME CHECKS FAILED (server logs: /tmp/inventory-n1.log, /tmp/inventory-n2.log)"; exit 1; }
