package store

import (
	"reflect"
	"strings"
	"testing"
)

// Acceptance criterion of #106: events are never updated after insert. No
// interface of the store offers a method that changes an event: the only
// methods naming events append, list or prune them. A new method on events
// must be added here on purpose.
func TestNoEventUpdateInStoreAPI(t *testing.T) {
	allowed := map[string]bool{"AppendEvent": true, "ListEventsAfter": true}
	for _, typ := range []reflect.Type{
		reflect.TypeFor[Tx](), reflect.TypeFor[Store](), reflect.TypeFor[ConfigTx](),
	} {
		for m := range typ.Methods() {
			if strings.Contains(m.Name, "Event") && !allowed[m.Name] {
				t.Errorf("%s.%s: a method on events that is not append or list", typ.Name(), m.Name)
			}
		}
	}
}
