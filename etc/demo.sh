#!/usr/bin/env sh
# Demonstration of the inventory service on real state: every operation and
# every documented error, in an order where later answers depend on earlier
# writes, then a run of concurrent writes.
#
# Start a FRESH server first (it loads the sample inventory: apple v3,
# banana v5, cherry v7, store revision 7), then run this once:
#   make run                                   # terminal 1
#   sh etc/demo.sh                             # terminal 2
# Through a second server (n2 forwards every call to n1): restart n1, then
#   make run2                                  # terminal 3
#   sh etc/demo.sh http://localhost:8081
# The comments show what a fresh server answers.
B=${1:-http://localhost:8080}
J='Content-Type: application/json'
set -x
# --- reads: GET -> ReadProducts -------------------------------------------
curl -si "$B/v1/products/apple?consistency=linearizable" | grep -iE '^(HTTP|etag)|^\{'  # 200, ETag "3"
curl -si "$B/v1/products/kiwi" | head -1                                     # 404
curl -s  "$B/v1/products?skuId=cherry&skuId=apple&skuId=kiwi&sort=-stock"   ; echo  # apple, cherry (kiwi skipped)
curl -s  "$B/v1/products?prefix=b&inStock=true&limit=1"                      ; echo  # banana
curl -si "$B/v1/products?skuId=apple&prefix=a" | head -1                     # 400: two selectors
curl -si "$B/v1/products?consistency=linearisable" | head -1                 # 400: unknown value
# --- create: POST -> CreateProduct ----------------------------------------
curl -si -X POST "$B/v1/products" -H "$J" -d '{"skuId":"kiwi","name":"Kiwi","priceCents":30,"stock":5}' | grep -iE '^(HTTP|etag|location)'  # 201, ETag "8"
curl -s  "$B/v1/products/kiwi"                                               ; echo  # 200: the new product is really there
curl -si -X POST "$B/v1/products" -H "$J" -d '{"skuId":"apple","name":"Apple","priceCents":50}' | head -1  # 409: exists
curl -si -X POST "$B/v1/products" -H "$J" -d '{"skuId":"","name":"Nothing","priceCents":1}' | head -1     # 400
# --- update: PATCH -> UpdateProduct ---------------------------------------
curl -s  -X PATCH "$B/v1/products/apple" -H "$J" -H 'If-Match: "3"' -d '{"priceCents":60}' ; echo     # 200, price 60, version 9
curl -si -X PATCH "$B/v1/products/apple" -H "$J" -H 'If-Match: "3"' -d '{"priceCents":70}' | head -1  # 412: apple moved on to 9 (no lost update)
curl -si -X PATCH "$B/v1/products/grape" -H "$J" -d '{"priceCents":60}' | head -1                     # 404
# --- read-modify-write: PATCH .../stock -> AddStock, POST /v1/orders -> PlaceOrder
curl -s  -X PATCH "$B/v1/products/cherry/stock" -H "$J" -d '{"add":5}'      ; echo  # 200, stock 0 -> 5
curl -si -X PATCH "$B/v1/products/cherry/stock" -H "$J" -d '{"add":0}' | head -1     # 400
curl -s  -X POST "$B/v1/orders" -H "$J" -d '{"items":[{"skuId":"apple","quantity":2},{"skuId":"banana","quantity":3}]}' ; echo  # 200: apple 8, banana 17, total 195
curl -si -X POST "$B/v1/orders" -H "$J" -d '{"items":[{"skuId":"apple","quantity":1},{"skuId":"cherry","quantity":6}]}' | head -1  # 409: only 5 cherries
curl -s  "$B/v1/products/apple"                                              ; echo  # stock still 8: the failed order changed nothing
curl -si -X POST "$B/v1/orders" -H "$J" -d '{"items":[{"skuId":"apple","quantity":0}]}' | head -1  # 400
# --- safe retries: Client-Id + Request-Seq ---------------------------------
curl -s  -X POST "$B/v1/orders" -H "$J" -H 'Client-Id: alice' -H 'Request-Seq: 1' -d '{"items":[{"skuId":"apple","quantity":1}]}' ; echo  # 200: apple 7
curl -s  -X POST "$B/v1/orders" -H "$J" -H 'Client-Id: alice' -H 'Request-Seq: 1' -d '{"items":[{"skuId":"apple","quantity":1}]}' ; echo  # 200: a retry, the same answer
curl -s  "$B/v1/products/apple"                                              ; echo  # stock 7: the retry was not applied again
curl -si -X POST "$B/v1/orders" -H "$J" -H 'Client-Id: alice' -H 'Request-Seq: 2' -d '{"items":[{"skuId":"apple","quantity":1}]}' | head -1  # 200: new, apple 6
curl -si -X POST "$B/v1/orders" -H "$J" -H 'Client-Id: alice' -H 'Request-Seq: 1' -d '{"items":[{"skuId":"apple","quantity":1}]}' | head -1  # 409: older than the last
curl -si -X POST "$B/v1/orders" -H "$J" -H 'Client-Id: alice' -d '{"items":[{"skuId":"apple","quantity":1}]}' | head -1  # 400: one header only
# --- delete: DELETE one, PATCH batch -> DeleteProducts ---------------------
curl -si -X DELETE "$B/v1/products/kiwi" -H 'If-Match: "9"' | head -1        # 412: kiwi is at 8
curl -si -X DELETE "$B/v1/products/kiwi" | head -1                           # 204
curl -si "$B/v1/products/kiwi" | head -1                                     # 404: gone
curl -si -X PATCH "$B/v1/products" -H "$J" -d '{"delete":["banana","kiwi"]}' | head -1   # 404: none of them
curl -si "$B/v1/products/banana" | head -1                                   # 200: banana was not deleted
curl -si -X PATCH "$B/v1/products" -H "$J" -d '{"delete":["banana","banana"]}' | head -1 # 400: twice
curl -si -X PATCH "$B/v1/products" -H "$J" -d '{"delete":["banana","cherry"]}' | head -1 # 204: all of them
curl -s  "$B/v1/products"                                                    ; echo  # only apple is left
# --- concurrent writes: 50 x add 1 at once ----------------------------------
set +x
( for i in $(seq 50); do curl -s -o /dev/null -X PATCH "$B/v1/products/apple/stock" -H "$J" -d '{"add":1}' & done; wait )
set -x
curl -s  "$B/v1/products/apple"                                              ; echo  # stock 6 + 50 = 56: no update was lost
# --- docs -----------------------------------------------------------------
curl -s  "$B/openapi.yaml" | head -3
curl -si "$B/docs"   | head -1                                               # Swagger UI (open in a browser)
curl -si "$B/editor" | head -1                                               # Swagger Editor (open in a browser)
