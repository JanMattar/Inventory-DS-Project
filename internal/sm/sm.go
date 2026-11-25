// PART II, STEP 2 — The command logic of the inventory.
//
// Package sm is the state machine: a state (the products, the store's
// revision counter, and the last write of every client), and commands that
// read it or change it. It is pure: no I/O, no clocks, no randomness, no
// goroutines, no locks, no globals. Every replica that starts from the same
// state and runs the same commands in the same order ends in the same state,
// with the same results (Part III relies on this).
//
//   - Execute runs one command on the state: it updates the state in place
//     and returns the Result for the client. It is the only code that
//     changes the state.
//   - A command that fails changes nothing: an order or a batch delete is
//     all or nothing. Every item is checked first; only then is anything
//     applied.
//   - Every successful write takes the next store revision, and every
//     product it touches gets that revision as its version.
//
// Products in the state are never modified, only replaced, so results may
// share them with the state.
package sm

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/your-team/inventory/gen/inventory/v1"
)

// NewState returns the empty state.
func NewState() *inventoryv1.State {
	return &inventoryv1.State{
		Products: map[string]*inventoryv1.Product{},
		Sessions: map[string]*inventoryv1.Session{},
	}
}

// Execute runs one command on s: it updates s and returns the Result.
// A failed command leaves s unchanged.
func Execute(s *inventoryv1.State, c *inventoryv1.Command) *inventoryv1.Result {
	if q, isRead := c.GetKind().(*inventoryv1.Command_Read); isRead {
		return read(s, q.Read) // a read changes nothing, and needs no retry check
	}
	if c.GetKind() == nil {
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "empty command")
	}

	// Safe retries: the same seq again is answered from the session, an
	// older one is refused. Neither runs the command.
	client, seq := c.GetClientId(), c.GetSeq()
	if (client == "") != (seq == 0) || seq < 0 {
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "give both a client id and a seq >= 1, or neither")
	}
	if last, ok := s.GetSessions()[client]; client != "" && ok {
		switch {
		case seq == last.GetLastSeq():
			return proto.Clone(last.GetResult()).(*inventoryv1.Result) // a retry: not applied again
		case seq < last.GetLastSeq():
			return fail(inventoryv1.Code_CODE_STALE_REQUEST,
				fmt.Sprintf("request %d of client %q is older than its last one (%d)", seq, client, last.GetLastSeq()))
		}
	}

	res := write(s, c)
	// Only an applied write is remembered: a failed one changed nothing, so
	// running it again on a retry is safe, and the state stays unchanged.
	if client != "" && res.GetCode() == inventoryv1.Code_CODE_OK {
		if s.Sessions == nil {
			s.Sessions = map[string]*inventoryv1.Session{}
		}
		s.Sessions[client] = &inventoryv1.Session{LastSeq: seq, Result: res}
	}
	return res
}

func write(s *inventoryv1.State, c *inventoryv1.Command) *inventoryv1.Result {
	switch k := c.GetKind().(type) {
	case *inventoryv1.Command_Create:
		return create(s, k.Create)
	case *inventoryv1.Command_Update:
		return update(s, k.Update)
	case *inventoryv1.Command_Delete:
		return deleteProducts(s, k.Delete.GetTargets())
	case *inventoryv1.Command_AddStock:
		return addStock(s, k.AddStock)
	case *inventoryv1.Command_PlaceOrder:
		return placeOrder(s, k.PlaceOrder.GetLines())
	}
	return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "unknown command")
}

// ---- snapshots ----

// TakeSnapshot copies the state into a Snapshot; applied = commands so far.
func TakeSnapshot(s *inventoryv1.State, applied int64) *inventoryv1.Snapshot {
	return &inventoryv1.Snapshot{State: proto.Clone(s).(*inventoryv1.State), Applied: applied}
}

// Restore returns the state stored in a snapshot, as a fresh copy. Each
// product's sku_id is taken from its map key, so a snapshot file need not
// repeat it.
func Restore(snap *inventoryv1.Snapshot) (*inventoryv1.State, int64) {
	s := NewState()
	s.Revision = snap.GetState().GetRevision()
	for k, p := range snap.GetState().GetProducts() {
		p = proto.Clone(p).(*inventoryv1.Product)
		p.SkuId = k
		s.Products[k] = p
	}
	for k, sess := range snap.GetState().GetSessions() {
		s.Sessions[k] = proto.Clone(sess).(*inventoryv1.Session)
	}
	return s, snap.GetApplied()
}

// ---- reads ----

func read(s *inventoryv1.State, q *inventoryv1.Read) *inventoryv1.Result {
	if q.GetLimit() < 0 {
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "limit must not be negative")
	}
	if _, ok := inventoryv1.SortOrder_Field_name[int32(q.GetSort().GetField())]; !ok {
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, fmt.Sprintf("unknown sort field %d", q.GetSort().GetField()))
	}
	var out []*inventoryv1.Product
	switch sel := q.GetSelect().(type) {
	case *inventoryv1.Read_SingleSkuId: // exactly one: missing is an error
		p, ok := s.GetProducts()[sel.SingleSkuId]
		if !ok {
			return fail(inventoryv1.Code_CODE_NOT_FOUND, fmt.Sprintf("product %q not found", sel.SingleSkuId))
		}
		return &inventoryv1.Result{Products: []*inventoryv1.Product{p}}
	case *inventoryv1.Read_SkuIdList: // several: missing ones are skipped, repeated ones returned once
		seen := map[string]bool{}
		for _, sku := range sel.SkuIdList.GetSkuIds() {
			if sku == "" {
				return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "skuId must not be empty")
			}
			if p, ok := s.GetProducts()[sku]; ok && !seen[sku] {
				out = append(out, p)
			}
			seen[sku] = true
		}
	case *inventoryv1.Read_Prefix:
		for k, p := range s.GetProducts() { // map order is random: sorted below
			if strings.HasPrefix(k, sel.Prefix) {
				out = append(out, p)
			}
		}
	case nil: // no selector: every product
		for _, p := range s.GetProducts() {
			out = append(out, p)
		}
	}
	if q.GetInStock() {
		out = slices.DeleteFunc(out, func(p *inventoryv1.Product) bool { return p.GetStock() <= 0 })
	}
	slices.SortFunc(out, func(a, b *inventoryv1.Product) int { return compare(a, b, q.GetSort()) })
	if l := int(q.GetLimit()); l > 0 && len(out) > l {
		out = out[:l]
	}
	return &inventoryv1.Result{Products: out}
}

// compare is a total order, so results never depend on Go's random map
// order. Ties are broken by sku_id, ascending (inventory.proto).
func compare(a, b *inventoryv1.Product, o *inventoryv1.SortOrder) int {
	c := 0
	switch o.GetField() {
	case inventoryv1.SortOrder_FIELD_NAME:
		c = strings.Compare(a.GetName(), b.GetName())
	case inventoryv1.SortOrder_FIELD_PRICE_CENTS:
		c = cmpInt(a.GetPriceCents(), b.GetPriceCents())
	case inventoryv1.SortOrder_FIELD_STOCK:
		c = cmpInt(a.GetStock(), b.GetStock())
	default:
		c = strings.Compare(a.GetSkuId(), b.GetSkuId())
	}
	if o.GetDescending() {
		c = -c
	}
	if c == 0 {
		c = strings.Compare(a.GetSkuId(), b.GetSkuId())
	}
	return c
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ---- writes ----
//
// Each write checks its input first, then the state, and only then changes
// anything; a check that fails returns before the first change.

func create(s *inventoryv1.State, c *inventoryv1.Create) *inventoryv1.Result {
	switch {
	case c.GetSkuId() == "" || c.GetName() == "":
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "skuId and name must not be empty")
	case c.GetPriceCents() < 0 || c.GetStock() < 0:
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "price and stock must not be negative")
	}
	if _, exists := s.GetProducts()[c.GetSkuId()]; exists {
		return fail(inventoryv1.Code_CODE_ALREADY_EXISTS, fmt.Sprintf("product %q already exists", c.GetSkuId()))
	}
	version := nextRevision(s)
	p := &inventoryv1.Product{SkuId: c.GetSkuId(), Name: c.GetName(), PriceCents: c.GetPriceCents(),
		Stock: c.GetStock(), Version: version}
	put(s, p)
	return ok(p)
}

func update(s *inventoryv1.State, u *inventoryv1.Update) *inventoryv1.Result {
	switch {
	case u.Name == nil && u.PriceCents == nil:
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "give name, priceCents or both")
	case u.Name != nil && u.GetName() == "":
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "name must not be empty")
	case u.PriceCents != nil && u.GetPriceCents() < 0:
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "price must not be negative")
	}
	old, f := find(s, u.GetSkuId(), u.ExpectedVersion)
	if f != nil {
		return f
	}
	p := proto.Clone(old).(*inventoryv1.Product) // a new product: the old one may be shared
	if u.Name != nil {
		p.Name = u.GetName()
	}
	if u.PriceCents != nil {
		p.PriceCents = u.GetPriceCents()
	}
	p.Version = nextRevision(s)
	put(s, p)
	return ok(p)
}

func deleteProducts(s *inventoryv1.State, targets []*inventoryv1.Target) *inventoryv1.Result {
	skus := make([]string, 0, len(targets))
	for _, t := range targets {
		skus = append(skus, t.GetSkuId())
	}
	if f := checkSkus(skus); f != nil {
		return f
	}
	for _, t := range targets { // check every product before deleting any
		if _, f := find(s, t.GetSkuId(), t.ExpectedVersion); f != nil {
			return f
		}
	}
	nextRevision(s) // a delete is a change too: it takes a revision
	for _, sku := range skus {
		delete(s.Products, sku)
	}
	return &inventoryv1.Result{}
}

func addStock(s *inventoryv1.State, a *inventoryv1.AddStock) *inventoryv1.Result {
	if a.GetAdd() < 1 {
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "add must be at least 1")
	}
	old, f := find(s, a.GetSkuId(), nil)
	if f != nil {
		return f
	}
	if old.GetStock() > math.MaxInt64-a.GetAdd() {
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "stock would overflow")
	}
	p := proto.Clone(old).(*inventoryv1.Product)
	p.Stock += a.GetAdd()
	p.Version = nextRevision(s)
	put(s, p)
	return ok(p)
}

// placeOrder checks every line, then applies all of them. A SKU may appear
// only once, so no line depends on the effect of an earlier one.
func placeOrder(s *inventoryv1.State, lines []*inventoryv1.OrderLine) *inventoryv1.Result {
	skus := make([]string, 0, len(lines))
	for _, l := range lines {
		if l.GetQuantity() < 1 {
			return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, fmt.Sprintf("%q: quantity must be at least 1", l.GetSkuId()))
		}
		skus = append(skus, l.GetSkuId())
	}
	if f := checkSkus(skus); f != nil {
		return f
	}
	olds := make([]*inventoryv1.Product, 0, len(lines))
	for _, l := range lines { // phase 1: check everything
		old, f := find(s, l.GetSkuId(), nil)
		if f != nil {
			return f
		}
		if l.GetQuantity() > old.GetStock() {
			return fail(inventoryv1.Code_CODE_OUT_OF_STOCK,
				fmt.Sprintf("%q: only %d in stock, %d ordered", l.GetSkuId(), old.GetStock(), l.GetQuantity()))
		}
		olds = append(olds, old)
	}
	version := nextRevision(s) // phase 2: apply everything, as one change
	res := &inventoryv1.Result{}
	for i, l := range lines {
		p := proto.Clone(olds[i]).(*inventoryv1.Product)
		p.Stock -= l.GetQuantity()
		p.Version = version
		put(s, p)
		res.Products = append(res.Products, p)
		res.TotalCents += l.GetQuantity() * p.GetPriceCents()
	}
	return res
}

// ---- helpers ----

// find returns the product, or a NOT_FOUND / VERSION_MISMATCH result. The
// version is checked only once the product is known to exist.
func find(s *inventoryv1.State, sku string, expectedVersion *int64) (*inventoryv1.Product, *inventoryv1.Result) {
	if sku == "" {
		return nil, fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "skuId must not be empty")
	}
	p, exists := s.GetProducts()[sku]
	if !exists {
		return nil, fail(inventoryv1.Code_CODE_NOT_FOUND, fmt.Sprintf("product %q not found", sku))
	}
	if expectedVersion != nil && *expectedVersion != p.GetVersion() {
		return nil, fail(inventoryv1.Code_CODE_VERSION_MISMATCH,
			fmt.Sprintf("product %q is at version %d, not %d", sku, p.GetVersion(), *expectedVersion))
	}
	return p, nil
}

// checkSkus checks a batch: not empty, no empty or repeated SKU.
func checkSkus(skus []string) *inventoryv1.Result {
	if len(skus) == 0 {
		return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "no products given")
	}
	seen := map[string]bool{}
	for _, sku := range skus {
		switch {
		case sku == "":
			return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, "skuId must not be empty")
		case seen[sku]:
			return fail(inventoryv1.Code_CODE_INVALID_ARGUMENT, fmt.Sprintf("skuId %q appears twice", sku))
		}
		seen[sku] = true
	}
	return nil
}

// nextRevision advances the store's counter. Called by a write only after
// all its checks passed, so a failed write never takes a revision.
func nextRevision(s *inventoryv1.State) int64 {
	s.Revision++
	return s.Revision
}

func put(s *inventoryv1.State, p *inventoryv1.Product) {
	if s.Products == nil { // a State parsed from empty text has no map yet
		s.Products = map[string]*inventoryv1.Product{}
	}
	s.Products[p.GetSkuId()] = p
}

func ok(p *inventoryv1.Product) *inventoryv1.Result {
	return &inventoryv1.Result{Products: []*inventoryv1.Product{p}}
}

func fail(c inventoryv1.Code, msg string) *inventoryv1.Result {
	return &inventoryv1.Result{Code: c, Message: msg}
}
