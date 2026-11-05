package watchfile

import (
	"context"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/subscription"
)

// One object at a time, for the REST API (story #71): a PUT body is the
// object of the watch file (spec docs/api.md), so it is validated by the same
// rules. JSON is YAML, so data may be either. The name comes from the request
// path: a "name" key in data is replaced by it (the caller checks that the two
// agree). Error paths are relative to the object ("policy.prerelease"), with
// a leading "." where the parser joins a key to the empty root path.

// ParseWatch validates one watch object. Warnings are dropped.
func ParseWatch(data []byte, name string, opts Options) (Watch, Errors) {
	p := &parser{opts: opts}
	root := p.object(data, name)
	if root == nil {
		return Watch{}, p.errs
	}
	w, _ := p.watch(root, "", map[string]int{})
	return w, p.errs
}

// ParseSink validates one sink object. Warnings are dropped.
func ParseSink(data []byte, name string, opts Options) (Sink, Errors) {
	p := &parser{opts: opts}
	root := p.object(data, name)
	if root == nil {
		return Sink{}, p.errs
	}
	s, _ := p.sink(root, "", map[string]int{})
	return s, p.errs
}

// ParseSubscription validates one subscription object. Whether its sink
// exists is the caller's check. Warnings are dropped.
func ParseSubscription(data []byte, name string, opts Options) (Subscription, Errors) {
	p := &parser{opts: opts}
	root := p.object(data, name)
	if root == nil {
		return Subscription{}, p.errs
	}
	s, _ := p.subscription(root, "", map[string]int{})
	return s, p.errs
}

// object decodes data into a mapping node whose "name" is name.
func (p *parser) object(data []byte, name string) *yaml.Node {
	root := p.document(data)
	if root == nil {
		return nil
	}
	if root.Kind != yaml.MappingNode {
		p.errf(root, "", "must be an object")
		return nil
	}
	content := make([]*yaml.Node, 0, len(root.Content))
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "name" {
			content = append(content, root.Content[i], root.Content[i+1])
		}
	}
	root.Content = append(content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name})
	return root
}

// ReevaluateHeld decides again the held deliveries of a subscription whose
// filter or policy was edited outside a file apply (the REST API), as Apply
// does for the file's subscriptions (ADR 0014 section 3), and returns a
// summary such as "1 held released". It writes through tx.
func ReevaluateHeld(ctx context.Context, tx store.ConfigTx, subID string, f subscription.Filter, p subscription.Policy, now time.Time) (string, error) {
	r := &reconciler{ctx: ctx, tx: tx, now: now, plan: &Plan{}}
	return r.reevaluateHeld(subID, f, p)
}
