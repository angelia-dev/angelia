package api

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/store"
)

// Stored secrets: /v1/secrets (story #75, ADR 0024 §7). Every operation
// needs the admin scope (the specification says so). A value is
// write-only: it is read from the request body, sealed, and never written
// to a response, a log, an audit record or a problem detail.

// secretJSON is the Secret schema: metadata only.
type secretJSON struct {
	Name         string           `json:"name"`
	Hosts        []string         `json:"hosts"`
	KeyID        string           `json:"key_id"`
	CreatedAt    timestamp        `json:"created_at"`
	UpdatedAt    timestamp        `json:"updated_at"`
	ReferencedBy []secretReferrer `json:"referenced_by"`
}

// secretReferrer is one *_ref key that names a secret.
type secretReferrer struct {
	Resource string `json:"resource"`
	Name     string `json:"name"`
	Pointer  string `json:"pointer"`
}

func secretJSONOf(sec store.Secret, refs []secretReferrer) secretJSON {
	hosts := sec.Hosts
	if hosts == nil {
		hosts = []string{}
	}
	if refs == nil {
		refs = []secretReferrer{}
	}
	return secretJSON{
		Name: sec.Name, Hosts: hosts, KeyID: sec.KeyID,
		CreatedAt: timestamp(sec.CreatedAt), UpdatedAt: timestamp(sec.UpdatedAt), ReferencedBy: refs,
	}
}

// isRefKey reports whether a configuration key names a stored secret.
func isRefKey(k string) bool { return strings.HasSuffix(k, "_ref") }

// eachRef calls fn for every *_ref key of v with a string value, with its
// JSON Pointer under prefix. Keys under skip pointers (free-form maps such
// as labels, whose keys are data) are not references.
func eachRef(v any, ptr string, skip map[string]bool, fn func(ptr string, val any)) {
	if skip[ptr] {
		return
	}
	switch x := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			p := pointerJoin(ptr, k)
			if isRefKey(k) && !skip[p] {
				fn(p, x[k])
				continue
			}
			eachRef(x[k], p, skip, fn)
		}
	case []any:
		for i, e := range x {
			eachRef(e, pointerJoin(ptr, strconv.Itoa(i)), skip, fn)
		}
	}
}

// sinkReferrers lists, per secret name, the sinks whose configuration names
// it with a *_ref key.
func sinkReferrers(sinks []store.Sink) map[string][]secretReferrer {
	out := map[string][]secretReferrer{}
	for _, k := range sinks {
		var cfg any
		if json.Unmarshal(k.ConfigEncrypted, &cfg) != nil {
			continue
		}
		eachRef(cfg, "/config", nil, func(p string, val any) {
			if name, ok := val.(string); ok {
				out[name] = append(out[name], secretReferrer{Resource: "sink", Name: k.Name, Pointer: p})
			}
		})
	}
	for name, refs := range out {
		slices.SortFunc(refs, func(a, b secretReferrer) int {
			return cmp.Or(cmp.Compare(a.Resource, b.Resource), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Pointer, b.Pointer))
		})
		out[name] = refs
	}
	return out
}

// referrers reads every sink outside a transaction and lists the references
// to each secret. Watches and subscriptions hold no *_ref key yet (their
// schemas are closed); registry credentials (#111) add theirs here.
func (s *Server) referrers(ctx context.Context) (map[string][]secretReferrer, error) {
	var all []store.Sink
	afterName, afterID := "", ""
	for {
		page, err := s.opts.Store.ListNamedSinks(ctx, afterName, afterID, 500)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < 500 {
			break
		}
		afterName, afterID = page[len(page)-1].Name, page[len(page)-1].ID
	}
	return sinkReferrers(all), nil
}

// txReferrers is referrers inside an API transaction, over every sink.
func txReferrers(ctx context.Context, tx store.APITx) (map[string][]secretReferrer, error) {
	var all []store.Sink
	for _, owner := range []string{store.ManagedByAPI, store.ManagedByFile} {
		ks, err := tx.ListManagedSinks(ctx, owner)
		if err != nil {
			return nil, err
		}
		all = append(all, ks...)
	}
	return sinkReferrers(all), nil
}

// ListSecrets implements StrictServerInterface.
func (s *Server) ListSecrets(ctx context.Context, req ListSecretsRequestObject) (ListSecretsResponseObject, error) {
	cur, p := decodeCursor(req.Params.Cursor, cursorSecrets)
	if p != nil {
		return nil, p
	}
	all, err := s.opts.Store.ListSecrets(ctx)
	if err != nil {
		return nil, err
	}
	items, next, err := pageByName(cur, pageLimit(req.Params.Limit),
		func(afterName, _ string, n int) ([]store.Secret, error) {
			i, _ := slices.BinarySearchFunc(all, afterName, func(sec store.Secret, name string) int {
				return cmp.Compare(sec.Name, name)
			})
			for i < len(all) && all[i].Name == afterName && afterName != "" {
				i++ // names are unique: the page starts after afterName
			}
			return all[i:min(i+n, len(all))], nil
		},
		func(sec store.Secret) (string, string) { return sec.Name, sec.ID },
		func(store.Secret) (bool, error) { return true, nil })
	if err != nil {
		return nil, err
	}
	refs, err := s.referrers(ctx)
	if err != nil {
		return nil, err
	}
	page := pageJSON[secretJSON]{Items: make([]secretJSON, 0, len(items)), NextCursor: next}
	for _, sec := range items {
		page.Items = append(page.Items, secretJSONOf(sec, refs[sec.Name]))
	}
	return jsonOK(page), nil
}

// GetSecret implements StrictServerInterface.
func (s *Server) GetSecret(ctx context.Context, req GetSecretRequestObject) (GetSecretResponseObject, error) {
	sec, err := s.opts.Store.SecretByName(ctx, req.Name)
	if sec, err = readByName(sec, err, "secret", req.Name); err != nil {
		return nil, err
	}
	refs, err := s.referrers(ctx)
	if err != nil {
		return nil, err
	}
	return jsonOK(secretJSONOf(sec, refs[sec.Name])), nil
}

// secretBody is a parsed PUT body. value is plaintext: it must never reach a
// log, an error or a response.
type secretBody struct {
	value secrets.Value
	hosts []string
}

// parseSecretBody applies the rules the schema cannot express. Errors name
// a member and, for a value, its size: never the value.
func parseSecretBody(name string, raw []byte) (secretBody, []fieldError) {
	errs := bodyNameErrors(raw, name)
	obj := rawObject(raw)
	var b secretBody
	text, hasText := obj["value"].(string)
	enc, hasEnc := obj["value_base64"].(string)
	var plain []byte
	decoded := false
	switch {
	case hasText && hasEnc:
		errs = append(errs, fieldError{Pointer: "", Detail: "give exactly one of value and value_base64"})
	case hasText:
		plain, decoded = []byte(text), true
	case hasEnc:
		d, err := base64.StdEncoding.Strict().DecodeString(enc)
		if err != nil {
			errs = append(errs, fieldError{Pointer: "/value_base64", Detail: "is not the standard base64 encoding (RFC 4648 section 4, padded)"})
		}
		plain, decoded = d, err == nil
	default:
		errs = append(errs, fieldError{Pointer: "", Detail: "give exactly one of value and value_base64"})
	}
	member := "/value"
	if hasEnc {
		member = "/value_base64"
	}
	switch {
	case decoded && len(plain) == 0:
		errs = append(errs, fieldError{Pointer: member, Detail: "must not be empty"})
	case len(plain) > secrets.MaxValueBytes:
		errs = append(errs, fieldError{Pointer: member, Detail: fmt.Sprintf("is %d bytes; a secret is at most %d bytes", len(plain), secrets.MaxValueBytes)})
	}
	b.value = secrets.NewValue(plain)
	clear(plain)
	b.hosts = []string{}
	if hs, ok := obj["hosts"].([]any); ok {
		for i, h := range hs {
			hv, _ := h.(string)
			if !secrets.ValidHost(hv) {
				errs = append(errs, fieldError{
					Pointer: "/hosts/" + strconv.Itoa(i),
					Detail:  "must be host or host:port, the host a lower-case hostname (punycode) or an IP literal, with no wildcard",
				})
				continue
			}
			b.hosts = append(b.hosts, hv)
		}
	}
	return b, errs
}

// PutSecret implements StrictServerInterface.
func (s *Server) PutSecret(ctx context.Context, req PutSecretRequestObject) (PutSecretResponseObject, error) {
	keys := s.opts.Secrets
	if keys == nil {
		return nil, problem(SlugSecretsKeyMissing, "the instance has no secrets key configured (secrets.key_file); nothing was stored")
	}
	b, errs := parseSecretBody(req.Name, stateFrom(ctx).body)
	if len(errs) > 0 {
		return nil, validationProblem("secret", errs)
	}
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	var body secretJSON
	var change Change
	var changed []string
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		cur, err := tx.SecretByName(ctx, req.Name)
		if err != nil && !isNotFound(err) {
			return err
		}
		now := s.now()
		var row store.Secret
		switch {
		case isNotFound(err):
			row = store.Secret{ID: s.opts.NewID(), Name: req.Name, Hosts: b.hosts, CreatedAt: now, UpdatedAt: now}
			if err := seal(keys, &row, b.value); err != nil {
				return err
			}
			if err := tx.CreateSecret(ctx, row); err != nil {
				return err
			}
			change, changed = ChangeCreated, []string{"value", "hosts"}
		default:
			row = cur
			if old, oerr := keys.Open(cur.ID, cur.Name, cur.Ciphertext); oerr != nil || !old.Equal(b.value.Reveal()) {
				changed = append(changed, "value")
			}
			if !slices.Equal(cur.Hosts, b.hosts) {
				changed = append(changed, "hosts")
			}
			if len(changed) == 0 {
				change = ChangeUnchanged
				break
			}
			row.Hosts, row.UpdatedAt = b.hosts, now
			if err := seal(keys, &row, b.value); err != nil {
				return err
			}
			ok, err := tx.UpdateSecret(ctx, row)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("secret %s changed during the request", req.Name)
			}
			row.Revision++
			change = ChangeUpdated
		}
		refs, err := txReferrers(ctx, tx)
		if err != nil {
			return err
		}
		body = secretJSONOf(row, refs[row.Name])
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !dry && change != ChangeUnchanged {
		action := "secret.create"
		if change == ChangeUpdated {
			action = "secret.update"
		}
		s.audit(ctx, action, slog.String("secret", req.Name), slog.String("key_id", body.KeyID),
			slog.Any("changed", changed), slog.Any("hosts", body.Hosts))
	}
	return putResponse(change, location("secrets", req.Name), body), nil
}

// seal encrypts v into row under the primary key, binding it to the row's
// id and name.
func seal(keys *secrets.Keyring, row *store.Secret, v secrets.Value) error {
	plain := v.Reveal()
	defer clear(plain)
	env, kid, err := keys.Seal(row.ID, row.Name, plain)
	if err != nil {
		return err
	}
	row.Ciphertext, row.KeyID = env, kid
	return nil
}

// DeleteSecret implements StrictServerInterface.
func (s *Server) DeleteSecret(ctx context.Context, req DeleteSecretRequestObject) (DeleteSecretResponseObject, error) {
	dry := req.Params.DryRun != nil && *req.Params.DryRun
	var keyID string
	err := s.inTx(ctx, dry, func(tx store.APITx) error {
		cur, err := tx.SecretByName(ctx, req.Name)
		if isNotFound(err) {
			return problem(SlugNotFound, "no secret named %s", req.Name)
		}
		if err != nil {
			return err
		}
		refs, err := txReferrers(ctx, tx)
		if err != nil {
			return err
		}
		if r := refs[req.Name]; len(r) > 0 {
			names := make([]string, len(r))
			for i, x := range r {
				names[i] = fmt.Sprintf("%s %s (%s)", x.Resource, x.Name, x.Pointer)
			}
			return problem(SlugSecretInUse, "secret %s is referenced by %s", req.Name, strings.Join(names, ", "))
		}
		keyID = cur.KeyID
		return tx.DeleteSecret(ctx, cur.ID)
	})
	if err != nil {
		return nil, err
	}
	if !dry {
		s.audit(ctx, "secret.delete", slog.String("secret", req.Name), slog.String("key_id", keyID))
	}
	return deleteResponse(), nil
}

// refScopeSkip lists the free-form maps of watch and subscription bodies:
// their keys are data, never configuration keys.
var refScopeSkip = map[string]bool{"/labels": true, "/coordinates": true, "/filter/labels": true}

// checkRefScope enforces ADR 0024 §2: a write that adds, removes or changes
// any *_ref key needs the admin scope, whatever the endpoint's scope. before
// and after are the resource before and after the write in the shape of its
// request body (nil when it does not exist). Writes that leave every *_ref
// unchanged keep the endpoint's scope.
func checkRefScope(ctx context.Context, before, after any) error {
	if principalFrom(ctx).Scope >= ScopeAdmin {
		return nil
	}
	refs := func(v any) map[string]string {
		out := map[string]string{}
		eachRef(v, "", refScopeSkip, func(p string, val any) { out[p] = canonical(val) })
		return out
	}
	if maps.Equal(refs(before), refs(after)) {
		return nil
	}
	p := problem(SlugForbidden, "this request adds, removes or changes a *_ref key (a reference to a stored secret), which needs the admin scope; the token has %s",
		principalFrom(ctx).Scope)
	p.RequiredScope = ScopeAdmin.String()
	return p
}

// asBody converts a response or stored value to the generic JSON shape the
// request body has, for checkRefScope.
func asBody(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	out, err := decodeJSON(b)
	if err != nil {
		return nil
	}
	return out
}

// CheckStoredSinks is the fail-closed upgrade check of ADR 0024 §6: it
// refuses a stored sink whose configuration holds a literal value at a path
// its kind's config_schema marks x-angelia-secret, which only *_ref keys
// may carry now. The error names the sink and the pointer, never the value.
func (s *Server) CheckStoredSinks(ctx context.Context) error {
	afterName, afterID := "", ""
	var bad []string
	for {
		page, err := s.opts.Store.ListNamedSinks(ctx, afterName, afterID, 500)
		if err != nil {
			return fmt.Errorf("api: check stored sinks: %w", err)
		}
		for _, k := range page {
			var cfg any
			if json.Unmarshal(k.ConfigEncrypted, &cfg) != nil {
				continue
			}
			sch, _ := s.configSchema(k.Kind)
			for _, p := range secretPaths(sch, nil, 0) {
				eachAt(cfg, p, nil, func(at []string, _ func(any)) {
					bad = append(bad, fmt.Sprintf("sink %s %s", k.Name, configPointer(at)))
				})
			}
		}
		if len(page) < 500 {
			break
		}
		afterName, afterID = page[len(page)-1].Name, page[len(page)-1].ID
	}
	if len(bad) > 0 {
		return fmt.Errorf("api: stored sink configuration holds secret values the kind marks x-angelia-secret (%s); store them with angelia secret set and reference them with a *_ref key",
			strings.Join(bad, ", "))
	}
	return nil
}

// Visit methods of the secret operations.

func (r response) VisitListSecretsResponse(w http.ResponseWriter) error  { return r.write(w) }
func (r response) VisitGetSecretResponse(w http.ResponseWriter) error    { return r.write(w) }
func (r response) VisitPutSecretResponse(w http.ResponseWriter) error    { return r.write(w) }
func (r response) VisitDeleteSecretResponse(w http.ResponseWriter) error { return r.write(w) }
