package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// Masked is the value of a configuration key the kind marks secret.
const Masked = "[REDACTED]"

// secretKeys returns the configuration keys a sink kind's config_schema marks
// with x-angelia-secret: true (spec docs/api.md decision 26).
func (s *Server) secretKeys(kind string) []string {
	pl, ok := s.plugin("sink", kind)
	if !ok || len(pl.ConfigSchema) == 0 {
		return nil
	}
	var sch struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if json.Unmarshal(pl.ConfigSchema, &sch) != nil {
		return nil
	}
	var out []string
	for k, p := range sch.Properties {
		if b, _ := p["x-angelia-secret"].(bool); b {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// sinkJSONOf builds the response of a sink for a caller with the given scope:
// below admin every URL in the configuration is cut to scheme://host, and for
// every caller the keys the kind marks secret are masked.
func (s *Server) sinkJSONOf(k store.Sink, scope Scope) sinkJSON {
	cfg := map[string]any{}
	_ = json.Unmarshal(k.ConfigEncrypted, &cfg) // TODO(#75): decrypt here once sinks are encrypted at rest
	out := sinkJSON{Name: k.Name, ID: k.ID, ManagedBy: ownerOf(k), Kind: k.Kind, Config: cfg, MaskedFields: []string{}}
	for _, key := range s.secretKeys(k.Kind) {
		if _, ok := cfg[key]; ok {
			cfg[key] = Masked
			out.MaskedFields = append(out.MaskedFields, key)
		}
	}
	if scope < ScopeAdmin {
		out.URLRedacted = true
		for key, v := range cfg {
			cfg[key] = redactURLs(v)
		}
	}
	return out
}

// redactURLs cuts every absolute URL in a configuration value to
// scheme://host.
func redactURLs(v any) any {
	switch x := v.(type) {
	case string:
		if u, err := url.Parse(x); err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
		if strings.Contains(x, "://") {
			return "[redacted]"
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = redactURLs(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = redactURLs(e)
		}
		return x
	}
	return v
}

// checkSinkConfig applies the API's own rules to a sink configuration: no key
// the kind marks secret, and every *_env name on the allowlist
// api.allowed_env (names starting with ANGELIA_ never).
func (s *Server) checkSinkConfig(kind string, cfg map[string]any) (invalid, env []fieldError) {
	for _, key := range s.secretKeys(kind) {
		if _, ok := cfg[key]; ok {
			invalid = append(invalid, fieldError{Pointer: pointerJoin("/config", key), Detail: "is a secret value and cannot be set through the API"})
		}
	}
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		name, ok := cfg[k].(string)
		if !strings.HasSuffix(k, "_env") || !ok {
			continue
		}
		if !s.opts.API.AllowsEnv(name) {
			env = append(env, fieldError{Pointer: pointerJoin("/config", k), Detail: "not allowed by api.allowed_env"})
		}
	}
	return invalid, env
}

// GetSink implements StrictServerInterface.
func (s *Server) GetSink(ctx context.Context, req GetSinkRequestObject) (GetSinkResponseObject, error) {
	k, err := s.opts.Store.SinkByName(ctx, req.Name)
	if k, err = readByName(k, err, "sink", req.Name); err != nil {
		return nil, err
	}
	return jsonOK(s.sinkJSONOf(k, principalFrom(ctx).Scope)), nil
}

// ListSinks implements StrictServerInterface.
func (s *Server) ListSinks(ctx context.Context, req ListSinksRequestObject) (ListSinksResponseObject, error) {
	cur, p := decodeCursor(req.Params.Cursor, cursorSinks)
	if p != nil {
		return nil, p
	}
	var kinds []string
	if req.Params.Kind != nil {
		kinds = *req.Params.Kind
	}
	items, next, err := pageByName(cur, pageLimit(req.Params.Limit),
		func(afterName, afterID string, n int) ([]store.Sink, error) {
			return s.opts.Store.ListNamedSinks(ctx, afterName, afterID, n)
		},
		func(k store.Sink) (string, string) { return k.Name, k.ID },
		func(k store.Sink) (bool, error) { return len(kinds) == 0 || slices.Contains(kinds, k.Kind), nil })
	if err != nil {
		return nil, err
	}
	scope := principalFrom(ctx).Scope
	page := pageJSON[sinkJSON]{Items: make([]sinkJSON, 0, len(items)), NextCursor: next}
	for _, k := range items {
		page.Items = append(page.Items, s.sinkJSONOf(k, scope))
	}
	return jsonOK(page), nil
}

// parseSinkBody validates a PUT body by the rules of the watch file and the
// API's own (checkSinkConfig). env holds the *_env names the allowlist
// refuses.
func (s *Server) parseSinkBody(name string, raw []byte) (k watchfile.Sink, invalid, env []fieldError) {
	invalid = bodyNameErrors(raw, name)
	data := truncateDurations(raw, []string{"config", "timeout"})
	k, perrs := watchfile.ParseSink(data, name, s.parseOptions())
	obj := rawObject(raw)
	kind, _ := obj["kind"].(string)
	cfg, _ := obj["config"].(map[string]any)
	secret, env := s.checkSinkConfig(kind, cfg)
	return k, append(append(invalid, parseErrors(perrs)...), secret...), env
}

// PutSink implements StrictServerInterface.
func (s *Server) PutSink(ctx context.Context, req PutSinkRequestObject) (PutSinkResponseObject, error) {
	k, fe, env := s.parseSinkBody(req.Name, stateFrom(ctx).body)
	if len(fe) > 0 {
		return nil, validationProblem("sink", append(fe, env...))
	}
	if len(env) > 0 {
		p := problem(SlugEnvNotAllowed, "config.%s names a variable that api.allowed_env does not allow", strings.TrimPrefix(env[0].Pointer, "/config/"))
		for _, e := range env {
			p.Errors = append(p.Errors, ProblemDetailError(e))
		}
		return nil, p
	}
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	var body sinkJSON
	var change Change
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		k0, err := tx.SinkByName(ctx, k.Name)
		cur, err := writeByName(k0, err, "sink", k.Name)
		if err != nil {
			return err
		}
		row := store.Sink{Kind: k.Kind, ConfigEncrypted: k.Config, Name: k.Name, ManagedBy: store.ManagedByAPI}
		switch {
		case cur == nil:
			row.ID = s.opts.NewID()
			// TODO(#75): encrypt the configuration at rest; the file apply
			// stores it as plain JSON too.
			if err := tx.CreateSink(ctx, row); err != nil {
				return err
			}
			change = ChangeCreated
		case cur.Kind != k.Kind || !bytes.Equal(cur.ConfigEncrypted, k.Config):
			row.ID = cur.ID
			if err := tx.UpdateSink(ctx, row); err != nil {
				return err
			}
			change = ChangeUpdated
		default:
			row.ID = cur.ID
			change = ChangeUnchanged
		}
		body = s.sinkJSONOf(row, principalFrom(ctx).Scope)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return putResponse(change, location("sinks", k.Name), body), nil
}

// DeleteSink implements StrictServerInterface.
func (s *Server) DeleteSink(ctx context.Context, req DeleteSinkRequestObject) (DeleteSinkResponseObject, error) {
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		k, err := tx.SinkByName(ctx, req.Name)
		cur, err := writeByName(k, err, "sink", req.Name)
		if err != nil {
			return err
		}
		if cur == nil {
			return problem(SlugNotFound, "no sink named %s", req.Name)
		}
		subs, err := tx.SubscriptionsBySink(ctx, cur.ID)
		if err != nil {
			return err
		}
		if len(subs) > 0 {
			return problem(SlugSinkInUse, "sink %s is used by subscription %s", req.Name, subs[0].Name)
		}
		return tx.DeleteSink(ctx, cur.ID)
	})
	if err != nil {
		return nil, err
	}
	return deleteResponse(), nil
}
