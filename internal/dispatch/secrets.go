package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
)

// SecretResolver resolves a stored secret: *secrets.Resolver. Resolve
// resolves it for one destination: a secret bound to hosts is resolved only
// when dest's host and port equal one of them (ADR 0024 §2).
// ResolveSelfAddressed resolves a secret whose value is the destination,
// with its hosts, for the dispatcher to check against the origins derived
// from it (ADR 0026 §2).
type SecretResolver interface {
	Resolve(ctx context.Context, name string, dest *url.URL) (secrets.Bound, error)
	ResolveSelfAddressed(ctx context.Context, name string) (secrets.Bound, error)
}

// errNoStoredSecrets is the error of a *_ref key when the dispatcher has no
// SecretResolver.
var errNoStoredSecrets = errors.New("stored secrets are not available to this dispatcher")

// SecretSources are where ResolveSecrets reads values from.
type SecretSources struct {
	// LookupEnv reads the variable named by each *_env key.
	LookupEnv func(string) (string, bool)
	// Stored resolves each *_ref key; nil fails every *_ref key.
	Stored SecretResolver
	// Destination is the URL the sink sends to: the configuration value the
	// kind's config_schema marks x-angelia-destination. A bound secret is
	// resolved only for it; nil refuses every bound secret.
	Destination *url.URL
	// SelfAddressed are the pointers of the keys the kind's config_schema
	// marks x-angelia-secret-destination (ADR 0026 §2). A *_ref key at one
	// of them is resolved with Stored.ResolveSelfAddressed, without the
	// host check, and comes back with its hosts for the caller to check.
	SelfAddressed []string
}

// selfAddressed is a resolved reference marked x-angelia-secret-destination.
type selfAddressed struct {
	ptr string
	// label names the reference for errors: "secret NAME" or "environment
	// variable NAME", never the value.
	label string
	bound secrets.Bound
}

// ResolveSecrets resolves the secret references of a sink configuration
// into the map of sink.Request.Secrets (ADR 0023 section 4, ADR 0024 §6):
//   - every key ending in "_env", at any depth, names an environment
//     variable read with src.LookupEnv;
//   - every key ending in "_ref" names a stored secret, resolved with
//     src.Stored for src.Destination.
//
// The map key is the RFC 6901 JSON Pointer of the reference key, such as
// "/secret_env" or "/secret_ref". A configuration without references gives
// a nil map; one that is not JSON is left to the sink to reject.
//
// A reference that is not a valid name, an unset or empty variable, a
// missing secret, a host the secret is not bound to or a secret that cannot
// be opened, or a variable starting with ANGELIA_, is an error naming the pointer and the variable or secret,
// never a value. The dispatcher retries it: the operator fixes it by
// setting the variable, storing the secret or changing its binding.
func ResolveSecrets(ctx context.Context, cfg []byte, src SecretSources) (map[string]secrets.Value, error) {
	out, _, err := resolveSecrets(ctx, cfg, src)
	return out, err
}

// resolveSecrets is ResolveSecrets that also returns the references at
// src.SelfAddressed, in pointer order, with their hosts.
func resolveSecrets(ctx context.Context, cfg []byte, src SecretSources) (map[string]secrets.Value, []selfAddressed, error) {
	var root any
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, nil, nil //nolint:nilerr // the sink reports an invalid configuration, permanently
	}
	var (
		out  map[string]secrets.Value
		self []selfAddressed
	)
	put := func(p string, v secrets.Value) {
		if out == nil {
			out = map[string]secrets.Value{}
		}
		out[p] = v
	}
	var walk func(v any, ptr string) error
	walk = func(v any, ptr string) error {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				p := ptr + "/" + escapePointer(k)
				var (
					val secrets.Value
					err error
				)
				marked := slices.Contains(src.SelfAddressed, p)
				switch {
				case strings.HasSuffix(k, "_env"):
					val, err = resolveEnv(x[k], p, src.LookupEnv)
					if err == nil && marked {
						name, _ := x[k].(string)
						self = append(self, selfAddressed{ptr: p, label: "environment variable " + name, bound: secrets.Bound{Name: name, Value: val}})
					}
				case strings.HasSuffix(k, "_ref") && marked:
					var b secrets.Bound
					b, err = resolveSelfAddressedRef(ctx, x[k], p, src.Stored)
					if err == nil {
						val = b.Value
						self = append(self, selfAddressed{ptr: p, label: "secret " + b.Name, bound: b})
					}
				case strings.HasSuffix(k, "_ref"):
					val, err = resolveRef(ctx, x[k], p, src)
				default:
					err = walk(x[k], p)
					if err != nil {
						return err
					}
					continue
				}
				if err != nil {
					return err
				}
				put(p, val)
			}
		case []any:
			for i, e := range x {
				if err := walk(e, ptr+"/"+strconv.Itoa(i)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, nil, err
	}
	return out, self, nil
}

func resolveEnv(v any, p string, lookupEnv func(string) (string, bool)) (secrets.Value, error) {
	name, ok := v.(string)
	if !ok || !config.IsEnvName(name) || config.LooksLikeToken(name) {
		return secrets.Value{}, fmt.Errorf("dispatch: secret %s: not an environment variable name", p)
	}
	if config.ReservedEnvName(name) { // a row stored before the write-time refusal
		return secrets.Value{}, fmt.Errorf("dispatch: secret %s: environment variable %s: %s", p, name, config.ReservedEnvRefusal)
	}
	val, _ := lookupEnv(name)
	if val == "" {
		return secrets.Value{}, fmt.Errorf("dispatch: secret %s: environment variable %s is not set or empty", p, name)
	}
	return secrets.NewValueString(val), nil
}

func resolveRef(ctx context.Context, v any, p string, src SecretSources) (secrets.Value, error) {
	name, ok := v.(string)
	if !ok || !secrets.ValidName(name) {
		return secrets.Value{}, fmt.Errorf("dispatch: secret %s: not a stored secret name", p)
	}
	if src.Stored == nil {
		return secrets.Value{}, fmt.Errorf("dispatch: secret %s: %w", p, errNoStoredSecrets)
	}
	b, err := src.Stored.Resolve(ctx, name, src.Destination)
	if err != nil {
		// The resolver's errors name the secret and the host, never the value.
		return secrets.Value{}, fmt.Errorf("dispatch: secret %s: %w", p, err)
	}
	return b.Value, nil
}

// resolveSelfAddressedRef resolves a *_ref key marked
// x-angelia-secret-destination: the secret with its hosts, unchecked.
func resolveSelfAddressedRef(ctx context.Context, v any, p string, stored SecretResolver) (secrets.Bound, error) {
	name, ok := v.(string)
	if !ok || !secrets.ValidName(name) {
		return secrets.Bound{}, fmt.Errorf("dispatch: secret %s: not a stored secret name", p)
	}
	if stored == nil {
		return secrets.Bound{}, fmt.Errorf("dispatch: secret %s: %w", p, errNoStoredSecrets)
	}
	b, err := stored.ResolveSelfAddressed(ctx, name)
	if err != nil {
		return secrets.Bound{}, fmt.Errorf("dispatch: secret %s: %w", p, err)
	}
	return b, nil
}

// destination returns the URL at the JSON Pointer ptr of a sink
// configuration (the key its kind marks x-angelia-destination), or nil when
// ptr is empty or the value is missing or not an absolute URL.
func destination(cfg []byte, ptr string) *url.URL {
	if ptr == "" {
		return nil
	}
	var v any
	if json.Unmarshal(cfg, &v) != nil {
		return nil
	}
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			v = x[tok]
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	s, _ := v.(string)
	u, err := url.Parse(s)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return nil
	}
	return u
}

// escapePointer escapes a key for a JSON Pointer (RFC 6901 section 3).
func escapePointer(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}
