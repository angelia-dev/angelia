package watchfile

import (
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/subscription"
)

const policyDoc = base + `
sinks:
  - {name: hook, kind: webhook, config: {url: 'https://h.example.org/x', secret_env: S}}
subscriptions:
`

func subDoc(policy string) string {
	return policyDoc + "  - {name: chat, sink: hook, filter: {bump: [minor]}, policy: " + policy + "}\n"
}

// AC: a subscription can set bump, prerelease and cooldown.
func TestSubscriptionPolicyKeys(t *testing.T) {
	f := mustParse(t, subDoc("{prerelease: none, cooldown: 72h}"))
	s := f.Subscriptions[0]
	if s.Policy.Prerelease != "none" || s.Policy.Cooldown != 72*time.Hour || len(s.Filter.Bump) != 1 {
		t.Fatalf("subscription = %+v", s)
	}
	if got := string(s.PolicyJSON()); got != `{"prerelease":"none","cooldown":"72h0m0s"}` {
		t.Fatalf("policy json = %s", got)
	}
	// Absent policy: inherit everything.
	f = mustParse(t, policyDoc+"  - {name: ci, sink: hook}\n")
	if got := string(f.Subscriptions[0].PolicyJSON()); got != `{"prerelease":"","cooldown":"0s"}` {
		t.Fatalf("default policy json = %s", got)
	}
}

// AC: narrowing only is enforced. Values that widen or do nothing load (they
// are clamped to nothing at evaluation, ADR 0014); syntax errors still fail.
func TestSubscriptionPolicyClampsAndNeverFailsOnUselessValues(t *testing.T) {
	for _, c := range []struct {
		policy string
		want   subscription.Policy
	}{
		{"{prerelease: include}", subscription.Policy{Prerelease: "include"}},
		{"{prerelease: only}", subscription.Policy{Prerelease: "only"}},
		{"{cooldown: 0s}", subscription.Policy{}},
		{"{cooldown: -5h}", subscription.Policy{}},
		{"{cooldown: 99999h}", subscription.Policy{Cooldown: MaxCooldown}},
		{"{cooldown: 1500ms}", subscription.Policy{Cooldown: time.Second}},
	} {
		f := mustParse(t, subDoc(c.policy))
		if got := f.Subscriptions[0].Policy; got != c.want {
			t.Errorf("%s = %+v, want %+v", c.policy, got, c.want)
		}
	}
	for policy, want := range map[string]string{
		"{prerelease: sometimes}": "subscriptions[0].policy.prerelease: must be one of",
		"{cooldown: soon}":        "subscriptions[0].policy.cooldown: invalid duration",
		"{bump: [major]}":         `subscriptions[0].policy.bump: unknown key`,
		"[]":                      "subscriptions[0].policy: must be a mapping",
	} {
		_, err := Parse([]byte(subDoc(policy)), "w.yaml", opts(t))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", policy, err, want)
		}
	}
}

func policyOf(t *testing.T, st store.Store) string {
	t.Helper()
	var out string
	if err := st.InConfigTx(t.Context(), func(tx store.ConfigTx) error {
		subs, err := tx.ListManagedSubscriptions(t.Context(), store.ManagedByFile)
		if len(subs) == 1 {
			out = string(subs[0].Policy)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSubscriptionPolicyIsStoredAndEditsAreUpdates(t *testing.T) {
	st := openStore(t)
	mustApply(t, st, subDoc("{cooldown: 72h}"))
	if got := policyOf(t, st); got != `{"prerelease":"","cooldown":"72h0m0s"}` {
		t.Fatalf("stored policy = %s", got)
	}
	if p := mustApply(t, st, subDoc("{cooldown: 72h}")); !p.Empty() {
		t.Fatalf("second apply must be a no-op:\n%s", p)
	}
	if p := mustApply(t, st, subDoc("{cooldown: 24h}")); p.Count(KindSubscription, Update) != 1 {
		t.Fatalf("policy edit must be an update:\n%s", p)
	}
	if got := policyOf(t, st); got != `{"prerelease":"","cooldown":"24h0m0s"}` {
		t.Fatalf("stored policy = %s", got)
	}
}

// A row written before the policy column existed holds '{}': it equals "no
// policy", so the first apply after the upgrade changes nothing.
func TestLegacyEmptyPolicyIsUnchanged(t *testing.T) {
	st := openStore(t)
	doc := policyDoc + "  - {name: ci, sink: hook}\n"
	mustApply(t, st, doc)
	if err := st.InConfigTx(t.Context(), func(tx store.ConfigTx) error {
		subs, _ := tx.ListManagedSubscriptions(t.Context(), store.ManagedByFile)
		subs[0].Policy = nil // stored as '{}'
		return tx.UpdateSubscription(t.Context(), subs[0])
	}); err != nil {
		t.Fatal(err)
	}
	if p := mustApply(t, st, doc); !p.Empty() {
		t.Fatalf("legacy row must be unchanged:\n%s", p)
	}
}
