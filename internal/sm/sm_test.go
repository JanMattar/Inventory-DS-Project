// PART II, STEP 3 — Testing the command logic.
//
// The logic has no I/O, so no server is needed: build a state, run a
// command, compare the Result's code and the state after it with what we
// expect. States and commands are written in protobuf's text format
// (prototext). Run with: go test ./internal/sm
package sm

import (
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/your-team/inventory/gen/inventory/v1"
)

// fixture returns a FRESH state on every call, so tests cannot change each
// other's input: three products, the store at revision 7, and a client
// "alice" whose last applied write was number 5.
func fixture(t *testing.T) *inventoryv1.State {
	return state(t, fixtureText)
}

const fixtureText = `
	revision: 7
	products { key: "apple"  value { name: "Apple"  price_cents: 50  stock: 10 version: 3 } }
	products { key: "banana" value { name: "Banana" price_cents: 25  stock: 20 version: 5 } }
	products { key: "cherry" value { name: "Cherry" price_cents: 400 stock: 0  version: 7 } }
	sessions { key: "alice"  value { last_seq: 5 result { } } }`

// state reads a State written in prototext and copies each map key into its
// product, so the text need not repeat the SKU.
func state(t *testing.T, text string) *inventoryv1.State {
	t.Helper()
	s := parse(t, text, &inventoryv1.State{})
	for k, p := range s.GetProducts() {
		p.SkuId = k
	}
	return s
}

// parse reads a message written in prototext into m.
func parse[M proto.Message](t *testing.T, text string, m M) M {
	t.Helper()
	if err := prototext.Unmarshal([]byte(text), m); err != nil {
		t.Fatalf("bad prototext %q: %v", text, err)
	}
	return m
}

// Short names for the codes in the table below.
const (
	OK         = inventoryv1.Code_CODE_OK
	NotFound   = inventoryv1.Code_CODE_NOT_FOUND
	Invalid    = inventoryv1.Code_CODE_INVALID_ARGUMENT
	Exists     = inventoryv1.Code_CODE_ALREADY_EXISTS
	OutOfStock = inventoryv1.Code_CODE_OUT_OF_STOCK
	Stale      = inventoryv1.Code_CODE_STALE_REQUEST
	Mismatch   = inventoryv1.Code_CODE_VERSION_MISMATCH
)

// The fixture's products and session, to build the expected states below.
const (
	apple  = `products { key: "apple"  value { name: "Apple"  price_cents: 50  stock: 10 version: 3 } }`
	banana = `products { key: "banana" value { name: "Banana" price_cents: 25  stock: 20 version: 5 } }`
	cherry = `products { key: "cherry" value { name: "Cherry" price_cents: 400 stock: 0  version: 7 } }`
	alice  = `sessions { key: "alice"  value { last_seq: 5 result { } } }`
)

func TestExecute(t *testing.T) {
	tests := []struct {
		name    string
		command string // a Command, in prototext
		code    inventoryv1.Code
		after   string // the State after it, in prototext; "" = unchanged
	}{
		// ---- reads: never change the state
		{"read one product", `read { single_sku_id: "apple" }`, OK, ``},
		{"read a missing product", `read { single_sku_id: "kiwi" }`, NotFound, ``},
		{"read a list: a missing SKU is skipped", `read { sku_id_list { sku_ids: "apple" sku_ids: "kiwi" } }`, OK, ``},
		{"read with an empty SKU in the list", `read { sku_id_list { sku_ids: "" } }`, Invalid, ``},
		{"read with a negative limit", `read { prefix: "" limit: -1 }`, Invalid, ``},
		{"read with an unknown sort field", `read { sort { field: 9 } }`, Invalid, ``},

		// ---- create
		{"create a product: revision 7 -> 8, the product gets version 8",
			`create { sku_id: "kiwi" name: "Kiwi" price_cents: 30 stock: 5 }`, OK,
			`revision: 8 ` + apple + banana + cherry + alice +
				`products { key: "kiwi" value { name: "Kiwi" price_cents: 30 stock: 5 version: 8 } }`},
		{"create an existing SKU", `create { sku_id: "apple" name: "Apple" price_cents: 50 }`, Exists, ``},
		{"create with an empty name", `create { sku_id: "kiwi" name: "" }`, Invalid, ``},
		{"create with a negative price", `create { sku_id: "kiwi" name: "Kiwi" price_cents: -1 }`, Invalid, ``},

		// ---- update
		{"update the price at the expected version",
			`update { sku_id: "apple" price_cents: 60 expected_version: 3 }`, OK,
			`revision: 8 ` + banana + cherry + alice +
				`products { key: "apple" value { name: "Apple" price_cents: 60 stock: 10 version: 8 } }`},
		{"update at a wrong version: 412", `update { sku_id: "apple" name: "Red" expected_version: 7 }`, Mismatch, ``},
		{"update a missing product", `update { sku_id: "kiwi" name: "Kiwi" }`, NotFound, ``},
		{"update with nothing to change", `update { sku_id: "apple" }`, Invalid, ``},
		{"update to an empty name", `update { sku_id: "apple" name: "" }`, Invalid, ``},

		// ---- delete (one or several, all or nothing)
		{"delete one product: it takes a revision",
			`delete { targets { sku_id: "banana" } }`, OK,
			`revision: 8 ` + apple + cherry + alice},
		{"delete two products",
			`delete { targets { sku_id: "apple" expected_version: 3 } targets { sku_id: "banana" } }`, OK,
			`revision: 8 ` + cherry + alice},
		{"delete whose last product is missing deletes nothing",
			`delete { targets { sku_id: "apple" } targets { sku_id: "kiwi" } }`, NotFound, ``},
		{"delete whose last version is wrong deletes nothing",
			`delete { targets { sku_id: "apple" } targets { sku_id: "banana" expected_version: 9 } }`, Mismatch, ``},
		{"delete the same SKU twice", `delete { targets { sku_id: "apple" } targets { sku_id: "apple" } }`, Invalid, ``},
		{"delete nothing", `delete { }`, Invalid, ``},

		// ---- add stock (read-modify-write)
		{"add stock: 0 + 5",
			`add_stock { sku_id: "cherry" add: 5 }`, OK,
			`revision: 8 ` + apple + banana + alice +
				`products { key: "cherry" value { name: "Cherry" price_cents: 400 stock: 5 version: 8 } }`},
		{"add zero", `add_stock { sku_id: "cherry" add: 0 }`, Invalid, ``},
		{"add to a missing product", `add_stock { sku_id: "kiwi" add: 5 }`, NotFound, ``},
		{"add until the stock overflows", `add_stock { sku_id: "apple" add: 9223372036854775807 }`, Invalid, ``},

		// ---- place an order (read-modify-write, all or nothing)
		{"order two products: both change, as one revision",
			`place_order { lines { sku_id: "apple" quantity: 2 } lines { sku_id: "banana" quantity: 3 } }`, OK,
			`revision: 8 ` + cherry + alice +
				`products { key: "apple"  value { name: "Apple"  price_cents: 50 stock: 8  version: 8 } }
				 products { key: "banana" value { name: "Banana" price_cents: 25 stock: 17 version: 8 } }`},
		{"order whose last line is out of stock changes nothing",
			`place_order { lines { sku_id: "apple" quantity: 2 } lines { sku_id: "cherry" quantity: 1 } }`, OutOfStock, ``},
		{"order whose last product is missing changes nothing",
			`place_order { lines { sku_id: "apple" quantity: 1 } lines { sku_id: "kiwi" quantity: 1 } }`, NotFound, ``},
		{"order a quantity of zero", `place_order { lines { sku_id: "apple" quantity: 0 } }`, Invalid, ``},
		{"order the same SKU twice",
			`place_order { lines { sku_id: "apple" quantity: 1 } lines { sku_id: "apple" quantity: 1 } }`, Invalid, ``},
		{"order nothing", `place_order { }`, Invalid, ``},

		// ---- safe retries (alice's last applied write was number 5)
		{"a new seq is applied and remembered",
			`client_id: "alice" seq: 6 add_stock { sku_id: "apple" add: 1 }`, OK,
			`revision: 8 ` + banana + cherry +
				`products { key: "apple" value { name: "Apple" price_cents: 50 stock: 11 version: 8 } }
				 sessions { key: "alice" value { last_seq: 6 result {
				   products { sku_id: "apple" name: "Apple" price_cents: 50 stock: 11 version: 8 } } } }`},
		{"the same seq again is a retry: not applied again",
			`client_id: "alice" seq: 5 add_stock { sku_id: "apple" add: 1 }`, OK, ``},
		{"an older seq is refused", `client_id: "alice" seq: 4 add_stock { sku_id: "apple" add: 1 }`, Stale, ``},
		{"a failed write is not remembered",
			`client_id: "bob" seq: 1 add_stock { sku_id: "kiwi" add: 1 }`, NotFound, ``},
		{"a client id without a seq", `client_id: "bob" add_stock { sku_id: "apple" add: 1 }`, Invalid, ``},
		{"an empty command", ``, Invalid, ``},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t)
			r := Execute(s, parse(t, tc.command, &inventoryv1.Command{}))
			if r.GetCode() != tc.code {
				t.Errorf("code %v (%s), want %v", r.GetCode(), r.GetMessage(), tc.code)
			}
			want := fixture(t) // "" = the state is unchanged
			if tc.after != "" {
				want = state(t, tc.after)
			}
			if !proto.Equal(s, want) {
				t.Errorf("state after:\n got %v\nwant %v", s, want)
			}
		})
	}
}

// Reads return the right products, in the right order.
func TestReadResults(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string // SKUs, in order
	}{
		{"all, by SKU", `read { }`, []string{"apple", "banana", "cherry"}},
		{"a list sorted by stock, descending; missing and repeated SKUs once or not at all",
			`read { sku_id_list { sku_ids: "cherry" sku_ids: "apple" sku_ids: "kiwi" sku_ids: "apple" }
			        sort { field: FIELD_STOCK descending: true } }`, []string{"apple", "cherry"}},
		{"a prefix", `read { prefix: "b" }`, []string{"banana"}},
		{"in stock only, by price, at most 1", `read { in_stock: true sort { field: FIELD_PRICE_CENTS } limit: 1 }`,
			[]string{"banana"}},
		{"by name, descending", `read { sort { field: FIELD_NAME descending: true } }`, []string{"cherry", "banana", "apple"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Execute(fixture(t), parse(t, tc.command, &inventoryv1.Command{}))
			var got []string
			for _, p := range r.GetProducts() {
				got = append(got, p.GetSkuId())
			}
			if r.GetCode() != OK || !equalStrings(got, tc.want) {
				t.Errorf("got %v %v, want %v", r.GetCode(), got, tc.want)
			}
		})
	}
}

// An order answers with the products after it and its total.
func TestOrderResult(t *testing.T) {
	r := Execute(fixture(t), parse(t,
		`place_order { lines { sku_id: "banana" quantity: 3 } lines { sku_id: "apple" quantity: 2 } }`, &inventoryv1.Command{}))
	want := parse(t, `total_cents: 175
		products { sku_id: "banana" name: "Banana" price_cents: 25 stock: 17 version: 8 }
		products { sku_id: "apple"  name: "Apple"  price_cents: 50 stock: 8  version: 8 }`, &inventoryv1.Result{})
	if !proto.Equal(r, want) {
		t.Fatalf("got %v, want %v", r, want)
	}
}

// A retry gets exactly the answer of the first try, and changes nothing.
func TestRetryReturnsSavedResult(t *testing.T) {
	s := fixture(t)
	c := parse(t, `client_id: "carol" seq: 1 place_order { lines { sku_id: "apple" quantity: 4 } }`, &inventoryv1.Command{})
	first := Execute(s, c)
	after := proto.Clone(s)
	second := Execute(s, c)
	if first.GetCode() != OK || !proto.Equal(first, second) {
		t.Fatalf("first %v, retry %v: want the same answer", first, second)
	}
	if !proto.Equal(s, after) || s.GetProducts()["apple"].GetStock() != 6 {
		t.Fatalf("the retry changed the state: apple stock %d, want 6", s.GetProducts()["apple"].GetStock())
	}
}

// Two copies of the fixture that run the same commands in the same order end
// in equal states, with equal results.
func TestDeterminism(t *testing.T) {
	cmds := []string{
		`create { sku_id: "kiwi" name: "Kiwi" price_cents: 30 stock: 5 }`,
		`client_id: "dave" seq: 1 place_order { lines { sku_id: "kiwi" quantity: 2 } lines { sku_id: "apple" quantity: 1 } }`,
		`read { }`,
		`update { sku_id: "banana" name: "Yellow banana" }`,
		`place_order { lines { sku_id: "cherry" quantity: 1 } }`, // out of stock
		`delete { targets { sku_id: "cherry" } }`,
		`add_stock { sku_id: "kiwi" add: 10 }`,
		`read { in_stock: true sort { field: FIELD_STOCK descending: true } }`,
	}
	a, b := fixture(t), fixture(t)
	for _, text := range cmds {
		ra := Execute(a, parse(t, text, &inventoryv1.Command{}))
		rb := Execute(b, parse(t, text, &inventoryv1.Command{}))
		if !proto.Equal(ra, rb) {
			t.Fatalf("%s: results differ:\n%v\n%v", text, ra, rb)
		}
	}
	if !proto.Equal(a, b) {
		t.Fatalf("states differ:\n%v\n%v", a, b)
	}
	// Reads do not depend on Go's random map order.
	for range 20 {
		if r := Execute(a, parse(t, `read { }`, &inventoryv1.Command{})); !proto.Equal(r, Execute(b, parse(t, `read { }`, &inventoryv1.Command{}))) {
			t.Fatalf("two reads of equal states differ")
		}
	}
}

// A state restored from a snapshot equals the state the snapshot was taken of.
func TestSnapshot(t *testing.T) {
	s := fixture(t)
	r := Execute(s, parse(t, `client_id: "erin" seq: 1 delete { targets { sku_id: "banana" } }`, &inventoryv1.Command{}))
	if r.GetCode() != OK {
		t.Fatalf("delete: %v", r)
	}
	back, applied := Restore(TakeSnapshot(s, 1))
	if !proto.Equal(back, s) || applied != 1 {
		t.Fatalf("restored %v (applied %d), want %v (1)", back, applied, s)
	}
	// A snapshot written by hand need not repeat the SKU inside each product.
	hand, _ := Restore(parse(t, `state { revision: 7 products { key: "apple" value { name: "Apple" version: 3 } } }`, &inventoryv1.Snapshot{}))
	if got := hand.GetProducts()["apple"].GetSkuId(); got != "apple" {
		t.Fatalf("restored sku_id %q, want apple", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
