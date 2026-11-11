package storetest

import (
	"reflect"
	"slices"
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
)

// TestEveryContractMethodHasACase compares the methods of the store
// interfaces, by reflection, with the list the suite keeps.
func TestEveryContractMethodHasACase(t *testing.T) {
	methods := map[string]bool{}
	for _, typ := range []reflect.Type{
		reflect.TypeFor[store.Store](), reflect.TypeFor[store.APIStore](), reflect.TypeFor[store.SecretStore](),
		// The transactions the stores hand to their callbacks.
		reflect.TypeFor[store.Tx](), reflect.TypeFor[store.ConfigTx](), reflect.TypeFor[store.APITx](), reflect.TypeFor[store.SecretTx](),
	} {
		for m := range typ.Methods() {
			methods[m.Name] = true
		}
	}
	caseNames := map[string]bool{}
	for _, c := range cases(Engine{}) {
		if caseNames[c.name] {
			t.Errorf("case %q is registered twice", c.name)
		}
		caseNames[c.name] = true
	}
	var missing []string
	for m := range methods {
		c, ok := covers[m]
		switch {
		case !ok:
			missing = append(missing, m)
		case !caseNames[c]:
			t.Errorf("method %s is covered by %q, which is not a case of the suite", m, c)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("methods without a storetest case (add one and list it in covers): %v", missing)
	}
	for m := range covers {
		if !methods[m] {
			t.Errorf("covers lists %s, which no store interface declares any more", m)
		}
	}
}
