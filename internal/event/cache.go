package event

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/subscription"
)

// SubscriptionReader is the part of store.Store that reads the subscriptions
// outside a transaction; *sqlite.Store implements it. [MatcherCache.Warm]
// uses it.
type SubscriptionReader interface {
	ListSubscriptions(ctx context.Context) ([]store.Subscription, error)
	SubscriptionGeneration(ctx context.Context) (int64, error)
}

// MatcherCache keeps the compiled subscription matcher and the decoded
// subscription policies between calls to [Emit], so that a check does not
// list, decode and compile every subscription inside its write transaction
// (about 87 ms at 10,000 subscriptions, measured in the #106 review).
//
// It is valid for exactly one store generation: the store counts every
// insert, update and delete of a subscription (store.Tx.SubscriptionGeneration),
// whichever code or process made it, and Emit reads that counter inside its
// transaction. A matching counter means the cached set is the committed set,
// so no per-id existence check is needed; a different one makes Emit reload
// inside the transaction (once per subscription change, not once per check).
// [MatcherCache.Warm] does that reload ahead of time, outside the
// transaction, so in steady state the transaction pays for a one-row read.
//
// A MatcherCache belongs to the component that calls Commit or Emit (the
// scheduler's check runner): create one with [NewMatcherCache] and pass it in
// [CheckInput.Matcher] or [EmitInput.Matcher]. There is no package-level
// cache. It is safe for concurrent use; the cached value is immutable.
type MatcherCache struct {
	mu  sync.Mutex
	cur *compiled
}

// NewMatcherCache returns an empty cache.
func NewMatcherCache() *MatcherCache { return &MatcherCache{} }

// compiled is the immutable result of loading the subscriptions at one
// generation.
type compiled struct {
	gen      int64
	matcher  *subscription.Matcher
	policies map[string]subscription.Policy
	invalid  []invalidSub
}

type invalidSub struct{ id, reason string }

// InvalidSubscription starts the LastError of the dead delivery Emit writes
// for a subscription whose stored filter or policy does not decode.
const InvalidSubscription = "invalid subscription filter/policy: "

// load compiles the subscriptions that decode, and lists those that do not,
// as of generation gen.
func load(ctx context.Context, subs []store.Subscription, gen int64, logger *slog.Logger) *compiled {
	var valid []subscription.Subscription
	c := &compiled{gen: gen, policies: make(map[string]subscription.Policy, len(subs))}
	for _, s := range subs {
		f, ferr := subscription.FromJSON(s.Filter)
		p, perr := subscription.PolicyFromJSON(s.Policy)
		if err := errors.Join(ferr, perr); err != nil {
			logger.ErrorContext(ctx, "invalid subscription; its deliveries of each event are dead",
				slog.String("subscription", s.ID), slog.String("error", err.Error()))
			c.invalid = append(c.invalid, invalidSub{id: s.ID, reason: InvalidSubscription + err.Error()})
			continue
		}
		valid = append(valid, subscription.Subscription{ID: s.ID, Filter: f})
		c.policies[s.ID] = p
	}
	c.matcher = subscription.NewMatcher(valid)
	return c
}

// Warm brings the cache to the store's current generation, reading through r
// outside any transaction. It is an optimisation: Emit is correct without it.
// The generation is read before the list, so a subscription written in
// between leaves the cache looking older than its content and Emit reloads;
// the cache is never labelled newer than what it holds.
func (c *MatcherCache) Warm(ctx context.Context, r SubscriptionReader, logger *slog.Logger) error {
	gen, err := r.SubscriptionGeneration(ctx)
	if err != nil {
		return fmt.Errorf("event: %w", err)
	}
	if cur := c.get(); cur != nil && cur.gen == gen {
		return nil
	}
	subs, err := r.ListSubscriptions(ctx)
	if err != nil {
		return fmt.Errorf("event: %w", err)
	}
	c.put(load(ctx, subs, gen, logger))
	return nil
}

func (c *MatcherCache) get() *compiled {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// put keeps the newest generation when two loaders race.
func (c *MatcherCache) put(n *compiled) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil || n.gen >= c.cur.gen {
		c.cur = n
	}
}

// current returns the compiled subscriptions as tx sees the store: the cached
// ones when the generation tx reads is the cached generation, otherwise it
// reloads inside tx. A nil cache always loads.
func (c *MatcherCache) current(ctx context.Context, tx store.Tx, logger *slog.Logger) (*compiled, error) {
	if c == nil {
		subs, err := tx.ListSubscriptions(ctx)
		if err != nil {
			return nil, fmt.Errorf("event: %w", err)
		}
		return load(ctx, subs, 0, logger), nil
	}
	gen, err := tx.SubscriptionGeneration(ctx)
	if err != nil {
		return nil, fmt.Errorf("event: %w", err)
	}
	if cur := c.get(); cur != nil && cur.gen == gen {
		return cur, nil
	}
	subs, err := tx.ListSubscriptions(ctx)
	if err != nil {
		return nil, fmt.Errorf("event: %w", err)
	}
	n := load(ctx, subs, gen, logger)
	c.put(n)
	return n, nil
}
