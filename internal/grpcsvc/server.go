// Package grpcsvc implements the Inventory gRPC service.
//
// PART II, STEP 4 — The service now has real state, kept by the state
// machine (internal/sm) and served by the executor (internal/executor). An
// RPC holds no state: it converts its request into ONE command
// (convert.go), submits it, waits for the result, and converts the result
// into its response or a gRPC status. In Part III the executor's queue
// becomes the replicated log; this file stays the same.
package grpcsvc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/prototext"

	inventoryv1 "github.com/your-team/inventory/gen/inventory/v1"
	"github.com/your-team/inventory/internal/executor"
	"github.com/your-team/inventory/internal/sm"
)

var snapshotFile = flag.String("snapshot", "", "start from this Snapshot, in prototext (e.g. etc/sample-snapshot.txtpb); empty = an empty inventory")

// Server implements inventoryv1.InventoryServer.
//
// Embedding UnimplementedInventoryServer makes every RPC not written yet
// return codes.Unimplemented (HTTP 501 after translation).
type Server struct {
	inventoryv1.UnimplementedInventoryServer

	NodeID string // sent back as `served-by`, so you can see WHICH server answered
	exec   *executor.Executor
}

// New starts the executor over the initial state (-snapshot, or empty) and
// returns the service.
func New(nodeID string) *Server {
	state := sm.NewState()
	if *snapshotFile != "" {
		var err error
		if state, err = LoadSnapshot(*snapshotFile); err != nil {
			log.Fatalf("[%s] snapshot: %v", nodeID, err)
		}
		log.Printf("[%s] state loaded from %s: %d products, revision %d",
			nodeID, *snapshotFile, len(state.GetProducts()), state.GetRevision())
	}
	exec := executor.New(state)
	go exec.Run(context.Background())
	return NewWith(nodeID, exec)
}

// NewWith returns the service over a running executor (tests).
func NewWith(nodeID string, exec *executor.Executor) *Server {
	return &Server{NodeID: nodeID, exec: exec}
}

// LoadSnapshot reads a Snapshot written in prototext and returns its state.
func LoadSnapshot(path string) (*inventoryv1.State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	snap := &inventoryv1.Snapshot{}
	if err := prototext.Unmarshal(b, snap); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	state, _ := sm.Restore(snap)
	return state, nil
}

// run submits a command, waits for its result and maps a failure to a status.
func (s *Server) run(ctx context.Context, cmd *inventoryv1.Command) (*inventoryv1.Result, error) {
	res, err := s.exec.Submit(ctx, cmd)
	if err != nil {
		return nil, status.FromContextError(err).Err() // timeout, or the executor stopped
	}
	return res, toStatus(res)
}

// runWrite is run for a write: it adds the retry metadata to the command.
func (s *Server) runWrite(ctx context.Context, cmd *inventoryv1.Command) (*inventoryv1.Result, error) {
	cmd, err := withRetry(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return s.run(ctx, cmd)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// ReadProducts is server streaming: one message per product, in sort order,
// then a message with fin = true (also when no product matched).
func (s *Server) ReadProducts(req *inventoryv1.ReadProductsRequest, stream inventoryv1.Inventory_ReadProductsServer) error {
	s.servedBy(stream.Context())
	if _, ok := inventoryv1.Consistency_name[int32(req.GetConsistency())]; !ok {
		return status.Errorf(codes.InvalidArgument, "unknown consistency %d", req.GetConsistency())
	}
	// The consistency level is accepted but not enforced yet: with one
	// server, every read is linearizable. Part III enforces it.
	res, err := s.run(stream.Context(), readCommand(req))
	if err != nil {
		return err
	}
	for _, p := range res.GetProducts() {
		if err := stream.Send(&inventoryv1.ReadProductsResponse{Product: p}); err != nil {
			return err
		}
	}
	return stream.Send(&inventoryv1.ReadProductsResponse{Fin: true}) // the stream is complete
}

// ---------------------------------------------------------------------------
// Writes: one command each, with the client's retry metadata.
// ---------------------------------------------------------------------------

func (s *Server) CreateProduct(ctx context.Context, req *inventoryv1.CreateProductRequest) (*inventoryv1.CreateProductResponse, error) {
	s.servedBy(ctx)
	res, err := s.runWrite(ctx, createCommand(req))
	if err != nil {
		return nil, err
	}
	return &inventoryv1.CreateProductResponse{Product: res.GetProducts()[0]}, nil
}

func (s *Server) UpdateProduct(ctx context.Context, req *inventoryv1.UpdateProductRequest) (*inventoryv1.UpdateProductResponse, error) {
	s.servedBy(ctx)
	res, err := s.runWrite(ctx, updateCommand(req))
	if err != nil {
		return nil, err
	}
	return &inventoryv1.UpdateProductResponse{Product: res.GetProducts()[0]}, nil
}

// DeleteProducts is client streaming: one message per product, all deleted
// by ONE command when the client closes the stream, or none.
func (s *Server) DeleteProducts(stream inventoryv1.Inventory_DeleteProductsServer) error {
	s.servedBy(stream.Context())
	deletes, err := recvAll[inventoryv1.DeleteProductsRequest](stream.Recv)
	if err != nil {
		return err
	}
	if _, err := s.runWrite(stream.Context(), deleteCommand(deletes)); err != nil {
		return err
	}
	return stream.SendAndClose(&inventoryv1.DeleteProductsResponse{})
}

// AddStock is a read-modify-write: the new stock depends on the old one.
func (s *Server) AddStock(ctx context.Context, req *inventoryv1.AddStockRequest) (*inventoryv1.AddStockResponse, error) {
	s.servedBy(ctx)
	res, err := s.runWrite(ctx, addStockCommand(req))
	if err != nil {
		return nil, err
	}
	return &inventoryv1.AddStockResponse{Product: res.GetProducts()[0]}, nil
}

// PlaceOrder is client streaming: one message per item, applied by ONE
// command when the client closes the stream. The whole order works or fails
// together.
func (s *Server) PlaceOrder(stream inventoryv1.Inventory_PlaceOrderServer) error {
	s.servedBy(stream.Context())
	items, err := recvAll[inventoryv1.PlaceOrderRequest](stream.Recv)
	if err != nil {
		return err
	}
	res, err := s.runWrite(stream.Context(), placeOrderCommand(items))
	if err != nil {
		return err
	}
	return stream.SendAndClose(&inventoryv1.PlaceOrderResponse{Products: res.GetProducts(), TotalCents: res.GetTotalCents()})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// servedBy tells the caller which node answered (metadata `served-by`).
func (s *Server) servedBy(ctx context.Context) {
	_ = grpc.SetHeader(ctx, metadata.Pairs("served-by", s.NodeID))
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
