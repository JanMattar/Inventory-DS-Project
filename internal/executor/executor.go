// PART II, STEP 4 — A single server with a command queue.
//
// Many goroutines (one per RPC) Submit commands; they go into a queue (a Go
// channel). One goroutine, Run, takes the commands from the queue one after
// another and executes each on the local state. Only Run ever touches the
// state, so no lock is needed. Each submitter waits until its command was
// executed, then returns its result.
//
//	RPC handler --Submit--> queue (channel) --next--> Run --sm.Execute--> state
//	     ^                                             |
//	     +--------------- reply channel <--------------+
//
// In Part III the queue is replaced by the replicated log; sm stays as it is.
package executor

import (
	"context"
	"errors"

	inventoryv1 "github.com/your-team/inventory/gen/inventory/v1"
	"github.com/your-team/inventory/internal/sm"
)

// QueueLen is the number of commands that may wait; Submit blocks when the
// queue is full.
const QueueLen = 1024

// ErrStopped is returned by Submit once Run has returned.
var ErrStopped = errors.New("executor stopped")

// A request is what travels through the queue: the command, and the channel
// on which Run sends back its result.
type request struct {
	cmd   *inventoryv1.Command
	reply chan *inventoryv1.Result
}

// newRequest wraps a command for the queue. The reply channel has capacity
// 1, so Run's send never blocks, even when the caller already gave up and
// nobody reads the reply: one slow caller cannot stop the whole executor.
func newRequest(cmd *inventoryv1.Command) request {
	return request{cmd: cmd, reply: make(chan *inventoryv1.Result, 1)}
}

// Executor owns the state and runs the queued commands on it, in order.
type Executor struct {
	state   *inventoryv1.State // only Run reads or writes it (no lock needed)
	queue   chan request       // RPCs write requests here; only Run reads them
	stopped chan struct{}      // closed when Run returns: tells Submit to stop waiting
}

// New returns an executor over the given state (sm.NewState() to start
// empty, or a state restored from a snapshot).
func New(state *inventoryv1.State) *Executor {
	return &Executor{
		state:   state,
		queue:   make(chan request, QueueLen), // buffered: Submit rarely waits
		stopped: make(chan struct{}),          // closed (never sent on) by Run
	}
}

// Submit enqueues cmd and waits for its result. Called by every RPC, from
// many goroutines at once.
func (e *Executor) Submit(ctx context.Context, cmd *inventoryv1.Command) (*inventoryv1.Result, error) {
	r := newRequest(cmd)
	if err := e.enqueue(ctx, r); err != nil {
		return nil, err // not queued, so never executed
	}
	return e.await(ctx, r)
}

// enqueue puts r into the queue, unless ctx ends or the executor stopped
// first. A plain `e.queue <- r` would block forever on a full queue or a
// stopped executor, ignoring the client's timeout.
func (e *Executor) enqueue(ctx context.Context, r request) error {
	select {
	case e.queue <- r:
		return nil // queued: Run will execute it
	case <-ctx.Done():
		return ctx.Err() // the client gave up first
	case <-e.stopped:
		return ErrStopped // Run has returned
	}
}

// await blocks until Run sends r's result, unless ctx ends or the executor
// stopped first. If ctx ends here, the command is already queued and still
// runs: the client cannot know whether its write happened. That is what the
// Client-Id / Request-Seq retry headers are for.
func (e *Executor) await(ctx context.Context, r request) (*inventoryv1.Result, error) {
	select {
	case res := <-r.reply:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.stopped:
		return nil, ErrStopped
	}
}

// Run executes the queued commands, one after another, until ctx ends.
// Call it once, in its own goroutine (go exec.Run(ctx)).
func (e *Executor) Run(ctx context.Context) {
	defer close(e.stopped) // however Run ends, every waiting Submit returns
	for {
		r, ok := e.next(ctx)
		if !ok {
			return // ctx ended: stop
		}
		e.execute(r)
	}
}

// next returns the next request, in queue order; ok is false once ctx has
// ended.
func (e *Executor) next(ctx context.Context) (request, bool) {
	select {
	case r := <-e.queue:
		return r, true
	case <-ctx.Done():
		return request{}, false
	}
}

// execute runs one command on the state and answers its submitter.
func (e *Executor) execute(r request) {
	res := sm.Execute(e.state, r.cmd) // the state changes here, and only here
	r.reply <- res                    // never blocks: the reply channel has capacity 1
}
