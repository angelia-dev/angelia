package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/angelia-dev/angelia/internal/store"
)

// GET /v1/access: the admin UI's access list, read-only (story #274, spec
// docs/api.md decision 50, ADR 0033 §3). It shows the entries of ui.access
// and the id a login entry is pinned to; never the OAuth configuration.

// accessProvider is the identity provider of every ui.access entry today
// (webauth.GitHubName; this package cannot import webauth).
const accessProvider = "github"

// accessKindUser is the kind of an entry that names one person.
const accessKindUser = "user"

// accessEntryJSON is the AccessEntry schema.
type accessEntryJSON struct {
	Provider string  `json:"provider"`
	Kind     string  `json:"kind"`
	Login    *string `json:"login"`
	Subject  *string `json:"subject"`
	Pinned   bool    `json:"pinned"`
	Role     string  `json:"role"`
}

type accessListJSON struct {
	Items []accessEntryJSON `json:"items"`
}

// ListAccess implements StrictServerInterface. A login entry's pin is read,
// never written: listing pins nobody.
func (s *Server) ListAccess(ctx context.Context, _ ListAccessRequestObject) (ListAccessResponseObject, error) {
	out := accessListJSON{Items: make([]accessEntryJSON, 0, len(s.opts.UIAccess))}
	for _, a := range s.opts.UIAccess {
		e := accessEntryJSON{Provider: accessProvider, Kind: accessKindUser, Role: a.Role}
		switch {
		case a.GitHubID != 0:
			sub := strconv.FormatInt(a.GitHubID, 10)
			e.Subject = &sub
		default:
			login := strings.ToLower(a.GitHubUser)
			e.Login = &login
			sub, err := s.opts.Store.UIIdentityPin(ctx, accessProvider, login)
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return nil, err
			default:
				e.Subject, e.Pinned = &sub, true
			}
		}
		out.Items = append(out.Items, e)
	}
	return jsonOK(out), nil
}

func (r response) VisitListAccessResponse(w http.ResponseWriter) error { return r.write(w) }
