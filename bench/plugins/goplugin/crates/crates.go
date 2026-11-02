// Package crates is the crates.io source of bench/plugins/crates/src/lib.rs
// ported to Go, line for line, as a go-plugin plugin (angelia-dev/angelia#60).
// Every HTTP request goes through the core's Host service, as in the WASM
// plugin; the plugin config is base_url (default https://crates.io) and an
// optional token, sent as the Authorization header.
package crates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/angelia-dev/angelia/bench/plugins/goplugin"
)

const (
	kind           = "crates"
	defaultBaseURL = "https://crates.io"
	maxPages       = 50
)

// Plugin implements goplugin.Plugin.
type Plugin struct {
	base  string
	token string
}

// Serve runs the plugin; call it from main.
func Serve() { goplugin.Serve(&Plugin{}) }

// Configure implements goplugin.Plugin.
func (p *Plugin) Configure(cfg map[string]string) error {
	for k := range cfg {
		if k != "base_url" && k != "token" {
			return fmt.Errorf("crates: unknown config key %q", k)
		}
	}
	p.base = strings.TrimRight(cfg["base_url"], "/")
	if p.base == "" {
		p.base = defaultBaseURL
	}
	p.token = cfg["token"]
	return nil
}

// Validate implements goplugin.Plugin.
func (p *Plugin) Validate(t goplugin.Target) string {
	_, err := checkName(t)
	if err != nil {
		return err.Error()
	}
	return ""
}

// checkName checks a crate name the way crates.io does: ASCII letters,
// digits, '-' and '_', starting with a letter, at most 64 characters.
func checkName(t goplugin.Target) (string, error) {
	if t.Kind != kind {
		return "", fmt.Errorf("kind %q is not %q", t.Kind, kind)
	}
	name := t.Coordinates["name"]
	if name == "" {
		return "", errors.New(`coordinate "name" is required`)
	}
	for k := range t.Coordinates {
		if k != "name" {
			return "", fmt.Errorf("unknown coordinate %q", k)
		}
	}
	if len(name) > 64 {
		return "", errors.New("crate name is longer than 64 characters")
	}
	if c := name[0]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
		return "", errors.New("crate name must start with an ASCII letter")
	}
	for _, c := range []byte(name) {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return "", errors.New("crate name may only hold ASCII letters, digits, '-' and '_'")
		}
	}
	return name, nil
}

type versionsPage struct {
	Versions []crateVersion `json:"versions"`
	Meta     struct {
		NextPage *string `json:"next_page"`
	} `json:"meta"`
}

type crateVersion struct {
	Num         string  `json:"num"`
	CreatedAt   string  `json:"created_at"`
	Yanked      bool    `json:"yanked"`
	Checksum    *string `json:"checksum"`
	License     *string `json:"license"`
	RustVersion *string `json:"rust_version"`
	DLPath      *string `json:"dl_path"`
}

// List implements goplugin.Plugin.
func (p *Plugin) List(ctx context.Context, host *goplugin.Host, in *goplugin.ListRequest) (*goplugin.ListResponse, error) {
	name, err := checkName(in.Target)
	if err != nil {
		return nil, err
	}
	first := p.base + "/api/v1/crates/" + name + "/versions"
	headers := map[string]string{"Accept": "application/json"}
	if p.token != "" {
		headers["Authorization"] = p.token
	}
	out := &goplugin.ListResponse{}
	url := first
	for page := range maxPages {
		req := goplugin.HTTPRequest{URL: url, Headers: maps.Clone(headers)}
		// Only the first page is conditional: if it is unchanged, so is the list.
		if page == 0 {
			req.Validators = &goplugin.Validators{ETag: in.State.ETag, LastModified: in.State.LastModified}
		}
		resp, err := host.HTTP(ctx, req)
		if err != nil {
			return nil, err
		}
		if e := resp.Error; e != nil {
			return nil, fmt.Errorf("%s: %s: %s", url, e.Code, e.Message)
		}
		if page == 0 {
			v := resp.Validators
			if resp.NotModified {
				return &goplugin.ListResponse{State: goplugin.State{ETag: v.ETag, LastModified: v.LastModified}, NotModified: true}, nil
			}
			out.State = goplugin.State{ETag: v.ETag, LastModified: v.LastModified}
		}
		switch resp.Status {
		case 200:
		case 404:
			return nil, fmt.Errorf("crate %q not found", name)
		default:
			return nil, fmt.Errorf("%s: unexpected status %d", url, resp.Status)
		}
		var body versionsPage
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			return nil, err
		}
		for _, v := range body.Versions {
			out.Versions = append(out.Versions, p.toVersion(name, v))
		}
		next := body.Meta.NextPage
		switch {
		case next != nil && strings.HasPrefix(*next, "?"):
			url = first + *next
		case next != nil && *next != "":
			return nil, fmt.Errorf("unexpected next_page %q", *next)
		default:
			return out, nil
		}
	}
	return nil, fmt.Errorf("crate %q: more than %d pages", name, maxPages)
}

func (p *Plugin) toVersion(name string, v crateVersion) goplugin.Version {
	md := map[string]any{}
	put := func(k string, v *string) {
		if v != nil {
			md[k] = *v
		}
	}
	put("checksum", v.Checksum)
	put("license", v.License)
	put("rust_version", v.RustVersion)
	if v.DLPath != nil {
		md["download_url"] = p.base + *v.DLPath
	}
	return goplugin.Version{
		Raw:         v.Num,
		PublishedAt: v.CreatedAt,
		Yanked:      v.Yanked,
		URL:         "https://crates.io/crates/" + name + "/" + v.Num,
		Metadata:    md,
	}
}
