// Package grpcsvc implements the Inventory gRPC service.
//
// STEP 4 — In Part I every RPC returns a DUMMY response: the goal is only to
// prove that the .proto compiles and that two processes can talk over gRPC.
// In Part II this type will apply operations to the state machine, and in
// Part III it will submit them to the replicated log.
package grpcsvc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	inventoryv1 "github.com/your-team/inventory/gen/inventory/v1"
)

// Server implements inventoryv1.InventoryServer.
//
// Embedding UnimplementedInventoryServer makes every RPC not written yet
// return codes.Unimplemented (HTTP 501 after translation).
type Server struct {
	inventoryv1.UnimplementedInventoryServer

	NodeID string // shown in dummy values, so you can see WHICH server answered
}

func New(nodeID string) *Server { return &Server{NodeID: nodeID} }

// dummyProduct is one product of the pretend store.
type dummyProduct struct {
	sku, name             string
	price, stock, version int64
}

// The pretend store. Any other SKU doesn't exist, and cherry is out of stock
// so you can see ?inStock=true work. Nothing is saved: a write just returns
// what the result would be, at version 8 (the store is at 7).
var dummyProducts = []dummyProduct{
	{"apple", "Apple", 50, 10, 3},
	{"banana", "Banana", 25, 20, 5},
	{"cherry", "Cherry", 400, 0, 7},
}

const dummyNextVersion = 8

// Pretend every client's last write was number 5.
const dummyLastSeq = 5

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// ReadProducts is server streaming: one ReadProductsResponse per product.
func (s *Server) ReadProducts(req *inventoryv1.ReadProductsRequest, stream inventoryv1.Inventory_ReadProductsServer) error {
	s.servedBy(stream.Context())
	_, ok := inventoryv1.Consistency_name[int32(req.GetConsistency())]
	if !ok {
		return status.Errorf(codes.InvalidArgument, "unknown consistency %d", req.GetConsistency())
	}
	_, ok = inventoryv1.SortOrder_Field_name[int32(req.GetSort().GetField())]
	if !ok {
		return status.Errorf(codes.InvalidArgument, "unknown sort field %d", req.GetSort().GetField())
	}
	if req.GetLimit() < 0 {
		return status.Error(codes.InvalidArgument, "limit must not be negative")
	}
	readNote := ", sequential read"
	if req.GetConsistency() == inventoryv1.Consistency_CONSISTENCY_LINEARIZABLE {
		readNote = ", linearizable read"
	}

	var products []dummyProduct
	switch selector := req.GetSelect().(type) {
	case *inventoryv1.ReadProductsRequest_SingleSku: // exactly one: missing is an error
		product, found := lookup(selector.SingleSku)
		if !found {
			return status.Errorf(codes.NotFound, "product %q not found", selector.SingleSku)
		}
		return stream.Send(&inventoryv1.ReadProductsResponse{Product: s.toProto(product, readNote)})
	case *inventoryv1.ReadProductsRequest_SkuList: // several: missing ones are skipped
		seen := map[string]bool{}
		for _, sku := range selector.SkuList.GetSkus() {
			if sku == "" {
				return status.Error(codes.InvalidArgument, "sku must not be empty")
			}
			product, found := lookup(sku)
			if found && !seen[sku] {
				products = append(products, product)
			}
			seen[sku] = true
		}
	case *inventoryv1.ReadProductsRequest_Prefix:
		for _, product := range dummyProducts {
			if strings.HasPrefix(product.sku, selector.Prefix) {
				products = append(products, product)
			}
		}
	case nil: // no selector: every product
		products = append(products, dummyProducts...)
	}

	// Then filter, sort, limit.
	if req.GetInStock() {
		var inStock []dummyProduct
		for _, product := range products {
			if product.stock > 0 {
				inStock = append(inStock, product)
			}
		}
		products = inStock
	}
	sortProducts(products, req.GetSort())
	limit := int(req.GetLimit())
	if limit > 0 && limit < len(products) {
		products = products[:limit]
	}
	for _, product := range products {
		err := stream.Send(&inventoryv1.ReadProductsResponse{Product: s.toProto(product, readNote)})
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Writes. Each one checks the retry headers first, then its input.
// ---------------------------------------------------------------------------

func (s *Server) CreateProduct(ctx context.Context, req *inventoryv1.CreateProductRequest) (*inventoryv1.CreateProductResponse, error) {
	s.servedBy(ctx)
	err := checkRetryHeaders(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetSku() == "" || req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "sku and name must not be empty")
	}
	if req.GetPriceCents() < 0 || req.GetStock() < 0 {
		return nil, status.Error(codes.InvalidArgument, "price and stock must not be negative")
	}
	_, exists := lookup(req.GetSku())
	if exists {
		return nil, status.Errorf(codes.AlreadyExists, "product %q already exists", req.GetSku())
	}
	product := dummyProduct{req.GetSku(), req.GetName(), req.GetPriceCents(), req.GetStock(), dummyNextVersion}
	return &inventoryv1.CreateProductResponse{Product: s.toProto(product, "")}, nil
}

func (s *Server) UpdateProduct(ctx context.Context, req *inventoryv1.UpdateProductRequest) (*inventoryv1.UpdateProductResponse, error) {
	s.servedBy(ctx)
	err := checkRetryHeaders(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case req.Name == nil && req.PriceCents == nil:
		return nil, status.Error(codes.InvalidArgument, "give name, priceCents or both")
	case req.Name != nil && *req.Name == "":
		return nil, status.Error(codes.InvalidArgument, "name must not be empty")
	case req.PriceCents != nil && *req.PriceCents < 0:
		return nil, status.Error(codes.InvalidArgument, "price must not be negative")
	}
	product, found := lookup(req.GetSku()) // a copy, so the pretend store never changes
	if !found {
		return nil, status.Errorf(codes.NotFound, "product %q not found", req.GetSku())
	}
	err = checkVersion(product, req.ExpectedVersion) // only once we know it exists
	if err != nil {
		return nil, err
	}
	if req.Name != nil {
		product.name = *req.Name
	}
	if req.PriceCents != nil {
		product.price = *req.PriceCents
	}
	product.version = dummyNextVersion
	return &inventoryv1.UpdateProductResponse{Product: s.toProto(product, "")}, nil
}

// DeleteProducts is client streaming: one DeleteProductsRequest per product.
func (s *Server) DeleteProducts(stream inventoryv1.Inventory_DeleteProductsServer) error {
	s.servedBy(stream.Context())
	deletes, err := recvAll[inventoryv1.DeleteProductsRequest](stream.Recv)
	if err != nil {
		return err
	}
	err = checkRetryHeaders(stream.Context())
	if err != nil {
		return err
	}
	var skus []string
	for _, toDelete := range deletes {
		skus = append(skus, toDelete.GetSku())
	}
	err = checkSkus(skus)
	if err != nil {
		return err
	}
	// Check every product before "deleting" anything: all or nothing.
	for _, toDelete := range deletes {
		product, found := lookup(toDelete.GetSku())
		if !found {
			return status.Errorf(codes.NotFound, "product %q not found", toDelete.GetSku())
		}
		err = checkVersion(product, toDelete.ExpectedVersion)
		if err != nil {
			return err
		}
	}
	return stream.SendAndClose(&inventoryv1.DeleteProductsResponse{})
}

// AddStock is a read-modify-write: the new stock depends on the old one.
func (s *Server) AddStock(ctx context.Context, req *inventoryv1.AddStockRequest) (*inventoryv1.AddStockResponse, error) {
	s.servedBy(ctx)
	err := checkRetryHeaders(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetAdd() < 1 {
		return nil, status.Error(codes.InvalidArgument, "add must be at least 1")
	}
	product, found := lookup(req.GetSku())
	if !found {
		return nil, status.Errorf(codes.NotFound, "product %q not found", req.GetSku())
	}
	if product.stock > math.MaxInt64-req.GetAdd() {
		return nil, status.Error(codes.InvalidArgument, "stock would overflow")
	}
	product.stock += req.GetAdd()
	product.version = dummyNextVersion
	return &inventoryv1.AddStockResponse{Product: s.toProto(product, "")}, nil
}

// PlaceOrder is client streaming: one PlaceOrderRequest per item. The whole
// order works or fails together.
func (s *Server) PlaceOrder(stream inventoryv1.Inventory_PlaceOrderServer) error {
	s.servedBy(stream.Context())
	orderItems, err := recvAll[inventoryv1.PlaceOrderRequest](stream.Recv)
	if err != nil {
		return err
	}
	err = checkRetryHeaders(stream.Context())
	if err != nil {
		return err
	}
	var skus []string
	for _, orderItem := range orderItems {
		if orderItem.GetQuantity() < 1 {
			return status.Errorf(codes.InvalidArgument, "%q: quantity must be at least 1", orderItem.GetSku())
		}
		skus = append(skus, orderItem.GetSku())
	}
	err = checkSkus(skus)
	if err != nil {
		return err
	}
	// Check every item before "applying" anything: all or nothing.
	var products []dummyProduct
	for _, orderItem := range orderItems {
		product, found := lookup(orderItem.GetSku())
		if !found {
			return status.Errorf(codes.NotFound, "product %q not found", orderItem.GetSku())
		}
		if orderItem.GetQuantity() > product.stock {
			return status.Errorf(codes.FailedPrecondition,
				"%q: only %d in stock, %d ordered", product.sku, product.stock, orderItem.GetQuantity())
		}
		products = append(products, product)
	}
	// Every item is fine: apply the whole order (on copies, nothing is saved).
	response := &inventoryv1.PlaceOrderResponse{}
	for i, orderItem := range orderItems {
		product := products[i]
		product.stock -= orderItem.GetQuantity()
		product.version = dummyNextVersion
		response.TotalCents += orderItem.GetQuantity() * product.price
		response.Products = append(response.Products, s.toProto(product, ""))
	}
	return stream.SendAndClose(response)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func lookup(sku string) (dummyProduct, bool) {
	for _, product := range dummyProducts {
		if product.sku == sku {
			return product, true
		}
	}
	return dummyProduct{}, false
}

func (s *Server) toProto(product dummyProduct, note string) *inventoryv1.Product {
	return &inventoryv1.Product{
		Sku:        product.sku,
		Name:       fmt.Sprintf("%s (dummy from %s%s)", product.name, s.NodeID, note),
		PriceCents: product.price,
		Stock:      product.stock,
		Version:    product.version,
	}
}

// servedBy tells the caller which node answered (metadata `served-by`).
func (s *Server) servedBy(ctx context.Context) {
	_ = grpc.SetHeader(ctx, metadata.Pairs("served-by", s.NodeID))
}

// just checking headers, no real retrying yet
func checkRetryHeaders(ctx context.Context) error {
	headers, _ := metadata.FromIncomingContext(ctx)
	clientIDs, requestSeqs := headers.Get("client-id"), headers.Get("request-seq")
	if len(clientIDs) == 0 && len(requestSeqs) == 0 {
		return nil // no retry headers, nothing to check
	}
	if len(clientIDs) == 0 || len(requestSeqs) == 0 {
		return status.Error(codes.InvalidArgument, "give both Client-Id and Request-Seq, or neither")
	}
	clientID := clientIDs[0]
	seqText := requestSeqs[0]
	seq, err := strconv.ParseInt(seqText, 10, 64)
	if clientID == "" || err != nil || seq < 1 {
		return status.Errorf(codes.InvalidArgument,
			"need a non-empty Client-Id and a Request-Seq >= 1, got %q and %q", clientID, seqText)
	}
	if seq < dummyLastSeq {
		return status.Errorf(codes.FailedPrecondition,
			"request %d of client %q is older than its last one (%d)", seq, clientID, dummyLastSeq)
	}
	return nil // 5 is a retry (a real server returns the saved answer), above 5 is new
}

// checkVersion checks an If-Match version against the product's.
func checkVersion(product dummyProduct, expectedVersion *int64) error {
	if expectedVersion != nil && *expectedVersion != product.version {
		return status.Errorf(codes.Aborted,
			"product %q is at version %d, not %d", product.sku, product.version, *expectedVersion)
	}
	return nil
}

// checkSkus checks a batch: not empty, no empty or repeated SKU.
func checkSkus(skus []string) error {
	if len(skus) == 0 {
		return status.Error(codes.InvalidArgument, "no products given")
	}
	seen := map[string]bool{}
	for _, sku := range skus {
		switch {
		case sku == "":
			return status.Error(codes.InvalidArgument, "sku must not be empty")
		case seen[sku]:
			return status.Errorf(codes.InvalidArgument, "sku %q appears twice", sku)
		default:
			seen[sku] = true
		}
	}
	return nil
}

// sortProducts sorts by the requested field; ties break by SKU, ascending.
func sortProducts(products []dummyProduct, order *inventoryv1.SortOrder) {
	sort.SliceStable(products, func(i, j int) bool {
		first, second := products[i], products[j]
		var comparison int
		switch order.GetField() {
		case inventoryv1.SortOrder_FIELD_NAME:
			comparison = strings.Compare(first.name, second.name)
		case inventoryv1.SortOrder_FIELD_PRICE_CENTS:
			comparison = cmp.Compare(first.price, second.price)
		case inventoryv1.SortOrder_FIELD_STOCK:
			comparison = cmp.Compare(first.stock, second.stock)
		default:
			comparison = strings.Compare(first.sku, second.sku)
		}
		if order.GetDescending() {
			comparison = -comparison
		}
		if comparison != 0 {
			return comparison < 0
		}
		return first.sku < second.sku
	})
}

// recvAll reads a client stream until the client closes it (io.EOF).
func recvAll[T any](recv func() (*T, error)) ([]*T, error) {
	var all []*T
	for {
		m, err := recv()
		if errors.Is(err, io.EOF) {
			return all, nil
		}
		if err != nil {
			return nil, err
		}
		all = append(all, m)
	}
}
