// PART II, STEP 5 — Testing the server's queue and executor.
//
// They use only New, Submit and Run. Run them with the race detector:
// go test -race ./internal/executor
//
//   - TestSameAsExecute: commands give the same results through the queue as
//     with sm.Execute directly (the tests of Step 3, now through the server);
//   - TestConcurrentWrites: many goroutines at once lose no update;
//   - TestConcurrentOrdersNeverOversell: concurrent orders never sell more
//     than the stock;
//   - TestTimeout / TestStopped: Submit gives up when its context ends, or
//     when Run has returned.
package executor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/your-team/inventory/gen/inventory/v1"
	"github.com/your-team/inventory/internal/sm"
)

func cmd(t *testing.T, text string) *inventoryv1.Command {
	t.Helper()
	c := &inventoryv1.Command{}
	if err := prototext.Unmarshal([]byte(text), c); err != nil {
		t.Fatal(err)
	}
	return c
}

// start runs an executor over the empty state until the test ends.
func start(t *testing.T) *Executor {
	ctx, cancel := context.WithCancel(context.Background())
	e := New(sm.NewState())
	go e.Run(ctx)
	t.Cleanup(cancel)
	return e
}

func submit(t *testing.T, e *Executor, c *inventoryv1.Command) *inventoryv1.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := e.Submit(ctx, c)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return r
}

func TestSameAsExecute(t *testing.T) {
	cmds := []string{
		`create { sku_id: "apple" name: "Apple" price_cents: 50 stock: 10 }`,
		`create { sku_id: "banana" name: "Banana" price_cents: 25 }`,
		`create { sku_id: "apple" name: "Apple" }`, // ALREADY_EXISTS
		`add_stock { sku_id: "banana" add: 4 }`,
		`client_id: "alice" seq: 1 place_order { lines { sku_id: "apple" quantity: 3 } lines { sku_id: "banana" quantity: 4 } }`,
		`client_id: "alice" seq: 1 place_order { lines { sku_id: "apple" quantity: 3 } lines { sku_id: "banana" quantity: 4 } }`, // a retry
		`place_order { lines { sku_id: "banana" quantity: 1 } }`,                                                                 // OUT_OF_STOCK
		`update { sku_id: "apple" price_cents: 60 expected_version: 1 }`,                                                         // VERSION_MISMATCH
		`read { sort { field: FIELD_STOCK descending: true } }`,
		`delete { targets { sku_id: "banana" } targets { sku_id: "kiwi" } }`, // NOT_FOUND
		`delete { targets { sku_id: "banana" } }`,
		`read { single_sku_id: "banana" }`, // NOT_FOUND
	}
	e, ref := start(t), sm.NewState()
	for _, c := range cmds {
		got := submit(t, e, cmd(t, c))
		if want := sm.Execute(ref, cmd(t, c)); !proto.Equal(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", c, got, want)
		}
	}
}

// Many goroutines add stock to the same product at once: no update is lost.
func TestConcurrentWrites(t *testing.T) {
	e := start(t)
	submit(t, e, cmd(t, `create { sku_id: "pen" name: "Pen" price_cents: 100 }`))
	const n = 200
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.Submit(context.Background(), cmd(t, `add_stock { sku_id: "pen" add: 1 }`))
			if err != nil || r.GetCode() != inventoryv1.Code_CODE_OK {
				t.Errorf("Submit: %v %v", r, err)
			}
		}()
	}
	wg.Wait()
	r := submit(t, e, cmd(t, `read { single_sku_id: "pen" }`))
	if got := r.GetProducts()[0]; got.GetStock() != n || got.GetVersion() != n+1 {
		t.Fatalf("pen stock %d version %d, want %d and %d: updates were lost", got.GetStock(), got.GetVersion(), n, n+1)
	}
}

// Concurrent orders for the last units: exactly the stock is sold, never more.
func TestConcurrentOrdersNeverOversell(t *testing.T) {
	e := start(t)
	submit(t, e, cmd(t, `create { sku_id: "ink" name: "Ink" price_cents: 10 stock: 50 }`))
	const n = 120
	var sold atomic.Int64
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.Submit(context.Background(), cmd(t, `place_order { lines { sku_id: "ink" quantity: 1 } }`))
			switch {
			case err != nil:
				t.Errorf("Submit: %v", err)
			case r.GetCode() == inventoryv1.Code_CODE_OK:
				sold.Add(1)
			case r.GetCode() != inventoryv1.Code_CODE_OUT_OF_STOCK:
				t.Errorf("order: %v", r)
			}
		}()
	}
	wg.Wait()
	r := submit(t, e, cmd(t, `read { single_sku_id: "ink" }`))
	if sold.Load() != 50 || r.GetProducts()[0].GetStock() != 0 {
		t.Fatalf("sold %d, stock left %d: want 50 and 0", sold.Load(), r.GetProducts()[0].GetStock())
	}
}

func TestTimeout(t *testing.T) {
	e := New(sm.NewState()) // Run not started: a command can never be executed
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.Submit(ctx, cmd(t, `read { }`)); err != context.DeadlineExceeded {
		t.Fatalf("Submit without Run: err = %v, want context.DeadlineExceeded", err)
	}
}

func TestStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := New(sm.NewState())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	cancel()
	<-done
	if _, err := e.Submit(context.Background(), cmd(t, `read { }`)); !errors.Is(err, ErrStopped) {
		t.Fatalf("Submit after Run returned: err = %v, want ErrStopped", err)
	}
}
