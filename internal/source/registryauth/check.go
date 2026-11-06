package registryauth

import (
	"fmt"
	"net/url"

	"github.com/angelia-dev/angelia/internal/secrets"
)

// Ref is one stored secret a source configuration names, and the registry
// URL it is sent to.
type Ref struct {
	// Key is the configuration key, such as "sources.npm.auth.token_ref".
	Key  string
	Name string
	URL  *url.URL
}

// CheckRefs checks, from metadata only (nothing is decrypted), that each
// referenced secret exists and that its hosts include its registry's host
// and port. hosts maps a secret name to its hosts. It returns one error per
// problem, in the order of refs.
func CheckRefs(refs []Ref, hosts map[string][]string) []error {
	var errs []error
	for _, r := range refs {
		h, ok := hosts[r.Name]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s: secret %s does not exist", r.Key, r.Name))
		case len(h) == 0:
			errs = append(errs, fmt.Errorf("%s: secret %s has no hosts; bind it to the registry host", r.Key, r.Name))
		default:
			if err := (secrets.Bound{Name: r.Name, Hosts: h}).Allows(r.URL); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.Key, err))
			}
		}
	}
	return errs
}
