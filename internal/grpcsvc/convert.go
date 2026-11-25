// PART II, STEP 1 — Converters between the API messages (inventory.proto)
// and the state machine's commands and results (sm.proto). Written by hand,
// one per RPC: the state machine never sees a gRPC request, and the API never
// sees a Command, so either can change without the other.
package grpcsvc

import (
	"context"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	inventoryv1 "github.com/your-team/inventory/gen/inventory/v1"
)

// readCommand: ReadProducts -> a Read command. The consistency level is not
// part of the command: it decides where a read is served (Part III), not
// what it returns.
func readCommand(r *inventoryv1.ReadProductsRequest) *inventoryv1.Command {
	q := &inventoryv1.Read{InStock: r.GetInStock(), Sort: r.GetSort(), Limit: r.GetLimit()}
	switch sel := r.GetSelect().(type) {
	case *inventoryv1.ReadProductsRequest_SingleSkuId:
		q.Select = &inventoryv1.Read_SingleSkuId{SingleSkuId: sel.SingleSkuId}
	case *inventoryv1.ReadProductsRequest_SkuIdList:
		q.Select = &inventoryv1.Read_SkuIdList{SkuIdList: sel.SkuIdList}
	case *inventoryv1.ReadProductsRequest_Prefix:
		q.Select = &inventoryv1.Read_Prefix{Prefix: sel.Prefix}
	}
	return &inventoryv1.Command{Kind: &inventoryv1.Command_Read{Read: q}}
}

// createCommand: CreateProduct -> a Create command.
func createCommand(r *inventoryv1.CreateProductRequest) *inventoryv1.Command {
	return &inventoryv1.Command{Kind: &inventoryv1.Command_Create{Create: &inventoryv1.Create{
		SkuId: r.GetSkuId(), Name: r.GetName(), PriceCents: r.GetPriceCents(), Stock: r.GetStock()}}}
}

// updateCommand: UpdateProduct -> an Update command (unset fields stay unset).
func updateCommand(r *inventoryv1.UpdateProductRequest) *inventoryv1.Command {
	return &inventoryv1.Command{Kind: &inventoryv1.Command_Update{Update: &inventoryv1.Update{
		SkuId: r.GetSkuId(), Name: r.Name, PriceCents: r.PriceCents, ExpectedVersion: r.ExpectedVersion}}}
}

// deleteCommand: the whole DeleteProducts stream -> ONE Delete command, so
// that a batch delete stays all or nothing.
func deleteCommand(reqs []*inventoryv1.DeleteProductsRequest) *inventoryv1.Command {
	d := &inventoryv1.Delete{}
	for _, r := range reqs {
		d.Targets = append(d.Targets, &inventoryv1.Target{SkuId: r.GetSkuId(), ExpectedVersion: r.ExpectedVersion})
	}
	return &inventoryv1.Command{Kind: &inventoryv1.Command_Delete{Delete: d}}
}

// addStockCommand: AddStock -> an AddStock command.
func addStockCommand(r *inventoryv1.AddStockRequest) *inventoryv1.Command {
	return &inventoryv1.Command{Kind: &inventoryv1.Command_AddStock{AddStock: &inventoryv1.AddStock{
		SkuId: r.GetSkuId(), Add: r.GetAdd()}}}
}

// placeOrderCommand: the whole PlaceOrder stream -> ONE PlaceOrder command.
func placeOrderCommand(reqs []*inventoryv1.PlaceOrderRequest) *inventoryv1.Command {
	o := &inventoryv1.PlaceOrder{}
	for _, r := range reqs {
		o.Lines = append(o.Lines, &inventoryv1.OrderLine{SkuId: r.GetSkuId(), Quantity: r.GetQuantity()})
	}
	return &inventoryv1.Command{Kind: &inventoryv1.Command_PlaceOrder{PlaceOrder: o}}
}

// withRetry copies the client-id and request-seq metadata of a write into
// its command: both, or neither. The state machine then decides whether the
// write is new, a retry, or stale.
func withRetry(ctx context.Context, cmd *inventoryv1.Command) (*inventoryv1.Command, error) {
	headers, _ := metadata.FromIncomingContext(ctx)
	clientIDs, requestSeqs := headers.Get("client-id"), headers.Get("request-seq")
	if len(clientIDs) == 0 && len(requestSeqs) == 0 {
		return cmd, nil // no retry headers
	}
	if len(clientIDs) == 0 || len(requestSeqs) == 0 {
		return nil, status.Error(codes.InvalidArgument, "give both Client-Id and Request-Seq, or neither")
	}
	seq, err := strconv.ParseInt(requestSeqs[0], 10, 64)
	if clientIDs[0] == "" || err != nil || seq < 1 {
		return nil, status.Errorf(codes.InvalidArgument,
			"need a non-empty Client-Id and a Request-Seq >= 1, got %q and %q", clientIDs[0], requestSeqs[0])
	}
	cmd.ClientId, cmd.Seq = clientIDs[0], seq
	return cmd, nil
}

// toStatus maps a Result's code to a gRPC status, in ONE place (the REST
// layer then maps that to HTTP, in httpStatusFromGRPC).
func toStatus(r *inventoryv1.Result) error {
	switch r.GetCode() {
	case inventoryv1.Code_CODE_OK:
		return nil
	case inventoryv1.Code_CODE_NOT_FOUND:
		return status.Error(codes.NotFound, r.GetMessage()) // 404
	case inventoryv1.Code_CODE_INVALID_ARGUMENT:
		return status.Error(codes.InvalidArgument, r.GetMessage()) // 400
	case inventoryv1.Code_CODE_ALREADY_EXISTS:
		return status.Error(codes.AlreadyExists, r.GetMessage()) // 409
	case inventoryv1.Code_CODE_OUT_OF_STOCK, inventoryv1.Code_CODE_STALE_REQUEST:
		return status.Error(codes.FailedPrecondition, r.GetMessage()) // 409
	case inventoryv1.Code_CODE_VERSION_MISMATCH:
		return status.Error(codes.Aborted, r.GetMessage()) // 412
	default:
		return status.Errorf(codes.Internal, "unknown result code %v: %s", r.GetCode(), r.GetMessage())
	}
}
