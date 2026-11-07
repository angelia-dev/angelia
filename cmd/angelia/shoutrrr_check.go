package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/sink/shoutrrr"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// shoutrrrWarnings checks the service URL of each shoutrrr sink of a watch
// file the way a delivery would derive it, without sending anything (ADR
// 0026 decided questions 4 and 7). It warns about a url_ref whose secret has
// no hosts, about an origin over plain http (credentials then travel in
// clear text), and about a URL a delivery would refuse. It reads url_env
// from env and url_ref from the store, read-only, with the configured key.
// The warnings name the sink, the reference, the service and the origin,
// never the URL.
func shoutrrrWarnings(ctx context.Context, cfg *config.Config, sinks []watchfile.Sink, lookupEnv func(string) (string, bool)) []string {
	var (
		out      []string
		resolver *secrets.Resolver
		closeDB  func() error
		storeErr error
	)
	defer func() {
		if closeDB != nil {
			_ = closeDB()
		}
	}()
	openResolver := func() (*secrets.Resolver, error) {
		if resolver != nil || storeErr != nil {
			return resolver, storeErr
		}
		keys, _, err := secrets.LoadKeyring(cfg.Secrets.KeyConfig())
		if err != nil {
			storeErr = err
			return nil, err
		}
		path, err := cfg.Database.SQLitePath()
		if err != nil {
			storeErr = errors.New("stored secrets are checked for sqlite databases only")
			return nil, storeErr
		}
		r, err := sqlite.OpenSecretReader(ctx, path)
		if err != nil {
			storeErr = err
			return nil, err
		}
		closeDB = r.Close
		resolver = &secrets.Resolver{Store: r, Keys: keys}
		return resolver, nil
	}
	sd := &shoutrrr.Sink{}
	for _, s := range sinks {
		if s.Kind != shoutrrr.Kind {
			continue
		}
		p, err := shoutrrr.ParseConfig(sink.Config(s.Config))
		if err != nil {
			continue // the watch file parser reported it
		}
		ref, ptr := p.Ref()
		warn := func(format string, a ...any) {
			out = append(out, fmt.Sprintf("sinks %s: %s: ", s.Name, ref)+fmt.Sprintf(format, a...))
		}
		var b secrets.Bound
		if p.URLEnv != "" {
			v, _ := lookupEnv(p.URLEnv)
			if v == "" {
				continue // the watch file parser warns about an unset variable
			}
			b = secrets.Bound{Name: p.URLEnv, Value: secrets.NewValueString(v)}
		} else {
			r, err := openResolver()
			if err != nil {
				warn("not checked: %v", err)
				continue
			}
			if b, err = r.ResolveSelfAddressed(ctx, p.URLRef); err != nil {
				warn("deliveries fail until this is fixed: %v", err)
				continue
			}
			if len(b.Hosts) == 0 {
				warn("the stored secret has no hosts: the service URL can be pointed at any host; set its hosts (angelia secret set --host)")
			}
		}
		origins, err := sd.SecretDestinations(ptr, b.Value)
		if err != nil {
			warn("deliveries fail until the URL is fixed: %v", err)
			continue
		}
		for _, o := range origins {
			if o.Scheme != "http" {
				continue
			}
			if slices.ContainsFunc(cfg.HTTP.AllowHTTPHosts, func(h string) bool { return strings.EqualFold(h, o.Hostname()) }) {
				warn("%s is plain http (disabletls): the service's credentials travel in clear text", o)
			} else {
				warn("%s is plain http (disabletls) and its host is not in http.allow_http_hosts: deliveries fail", o)
			}
		}
	}
	return out
}
