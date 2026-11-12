// Command server runs one node: a gRPC server (internal API) and an HTTP
// server (client REST API) whose handlers call a gRPC target.
//
//	go run ./cmd/server -id n1 -http :8080 -grpc :50051
//	go run ./cmd/server -id n2 -http :8081 -grpc :50052 -target localhost:50051
//
// The second command shows "forwarding": REST calls to n2 are answered by n1.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"

	"github.com/your-team/inventory/api"
	inventoryv1 "github.com/your-team/inventory/gen/inventory/v1"
	"github.com/your-team/inventory/internal/grpcsvc"
	"github.com/your-team/inventory/internal/httpapi"
)

func main() {
	id := flag.String("id", "n1", "node id")
	httpAddr := flag.String("http", ":8080", "client REST listen address")
	grpcAddr := flag.String("grpc", ":50051", "internal gRPC listen address")
	target := flag.String("target", "", "gRPC address the REST handlers call (default: this node)")
	corsOrigin := flag.String("cors-origin", "", `allowed CORS origin for browser tools on another port, e.g. "*" (dev only; empty = off)`)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- gRPC server (step 4) ----------------------------------------------
	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", *grpcAddr, err)
	}
	gs := grpc.NewServer()
	inventoryv1.RegisterInventoryServer(gs, grpcsvc.New(*id))
	reflection.Register(gs) // lets you poke the server with grpcurl
	go func() {
		log.Printf("[%s] gRPC listening on %s", *id, *grpcAddr)
		if err := gs.Serve(lis); err != nil {
			log.Fatalf("grpc serve: %v", err)
		}
	}()

	// --- gRPC client used by the REST layer (step 5) -----------------------
	if *target == "" {
		*target = "localhost" + portOf(*grpcAddr)
	}
	conn, err := grpc.NewClient(*target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("grpc client %s: %v", *target, err)
	}
	defer conn.Close()

	// --- HTTP server (steps 2 + 5) -----------------------------------------
	mux := http.NewServeMux()
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(api.Spec)
	})
	// Swagger UI (API docs + "Try it out"), same origin as the API -> no CORS.
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(api.DocsHTML)
	})
	// Swagger Editor (edit + preview). It cannot save to disk: File -> Save as YAML.
	mux.HandleFunc("GET /editor", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(api.EditorHTML)
	})
	hs := &http.Server{
		Addr:              *httpAddr,
		Handler:           withCORS(*corsOrigin, httpapi.NewHTTPHandler(inventoryv1.NewInventoryClient(conn), mux)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("[%s] REST listening on %s -> gRPC %s (docs: /docs, editor: /editor)", *id, *httpAddr, *target)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http serve: %v", err)
		}
	}()

	// --- graceful shutdown -------------------------------------------------
	<-ctx.Done()
	log.Printf("[%s] shutting down", *id)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(shutdownCtx)
	gs.GracefulStop()
}

// portOf turns ":50051" or "0.0.0.0:50051" into ":50051".
func portOf(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return ":" + port
}

// withCORS lets browser tools served from another origin (e.g. the Swagger
// Editor/UI and Scalar containers on :8090-:8092) call this API. Not needed for /docs,
// which is served from the same origin. An empty origin disables it.
func withCORS(origin string, next http.Handler) http.Handler {
	if origin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, If-Match, Client-Id, Request-Seq")
		h.Set("Access-Control-Expose-Headers", "ETag, Location") // let browser tools read them
		if r.Method == http.MethodOptions {                      // preflight
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
