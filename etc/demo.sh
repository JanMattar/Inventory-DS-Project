#!/usr/bin/env sh
# Smoke test of the inventory service: one curl per operation and per error.
# Start `make run` (and optionally `make run2`) first.
# Usage: ./etc/demo.sh [base-url]     default: http://localhost:8080
B=${1:-http://localhost:8080}
J='Content-Type: application/json'
set -x
# --- reads: GET -> ReadProducts -------------------------------------------
curl -si "$B/v1/products/apple?consistency=linearizable" | grep -iE '^(HTTP|etag)|^\{'  # 200 + ETag
curl -si "$B/v1/products/kiwi" | head -1                                 # 404
curl -s  "$B/v1/products?sku=cherry&sku=apple&sku=kiwi&sort=-stock"      ; echo  # kiwi skipped
curl -s  "$B/v1/products?prefix=b&inStock=true&limit=1"                  ; echo
curl -si "$B/v1/products?sku=apple&prefix=a" | head -1                   # 400: two selectors
curl -si "$B/v1/products?consistency=linearisable" | head -1             # 400: unknown value
# --- create: POST -> CreateProduct ----------------------------------------
curl -si -X POST "$B/v1/products" -H "$J" -d '{"sku":"kiwi","name":"Kiwi","priceCents":30,"stock":5}' | grep -iE '^(HTTP|etag|location)'  # 201
curl -si -X POST "$B/v1/products" -H "$J" -d '{"sku":"apple","name":"Apple","priceCents":50}' | head -1  # 409
# --- update: PATCH -> UpdateProduct ---------------------------------------
curl -s  -X PATCH "$B/v1/products/apple" -H "$J" -H 'If-Match: "3"' -d '{"priceCents":60}' ; echo  # 200
curl -si -X PATCH "$B/v1/products/apple" -H "$J" -H 'If-Match: "7"' -d '{"priceCents":60}' | head -1  # 412
curl -si -X PATCH "$B/v1/products/kiwi"  -H "$J" -d '{"priceCents":60}' | head -1  # 404
# --- delete: DELETE -> DeleteProducts -------------------------------------
curl -si -X DELETE "$B/v1/products/apple" | head -1                      # 204
curl -si -X DELETE "$B/v1/products/apple" -H 'If-Match: "9"' | head -1   # 412
curl -si -X DELETE "$B/v1/products?sku=apple&sku=banana" | head -1       # 204: all of them
curl -si -X DELETE "$B/v1/products?sku=apple&sku=kiwi" | head -1         # 404: none of them
# --- read-modify-write: PATCH .../stock -> AddStock, POST /v1/orders -> PlaceOrder
curl -s  -X PATCH "$B/v1/products/cherry/stock" -H "$J" -d '{"add":5}'  ; echo  # 200
curl -s  -X POST "$B/v1/orders" -H "$J" -d '{"items":[{"sku":"apple","quantity":2},{"sku":"banana","quantity":3}]}' ; echo  # 200
curl -si -X POST "$B/v1/orders" -H "$J" -d '{"items":[{"sku":"cherry","quantity":1}]}' | head -1  # 409: out of stock
curl -si -X POST "$B/v1/orders" -H "$J" -d '{"items":[{"sku":"apple","quantity":0}]}' | head -1   # 400
# --- safe retries: the dummy pretends alice's last write was number 5 ------
curl -si -X POST "$B/v1/orders" -H "$J" -H 'Client-Id: alice' -H 'Request-Seq: 6' -d '{"items":[{"sku":"apple","quantity":1}]}' | head -1  # 200: new
curl -si -X POST "$B/v1/orders" -H "$J" -H 'Client-Id: alice' -H 'Request-Seq: 4' -d '{"items":[{"sku":"apple","quantity":1}]}' | head -1  # 409: old
# --- docs -----------------------------------------------------------------
curl -s  "$B/openapi.yaml" | head -3
curl -si "$B/docs"   | head -1                                           # Swagger UI (open in a browser)
curl -si "$B/editor" | head -1                                           # Swagger Editor (open in a browser)
