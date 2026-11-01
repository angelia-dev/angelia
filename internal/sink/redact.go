package sink

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// urlPattern finds URLs with a scheme in free text. It stops at white space
// and quotes, which is how url.Error and most messages delimit them.
var urlPattern = regexp.MustCompile("[A-Za-z][A-Za-z0-9+.-]*://[^\\s\"'<>`]+")

// RedactedPath replaces the path and query of a redacted URL.
const RedactedPath = "/[redacted]"

// RedactURL returns err's message with every URL cut down to its scheme and
// host: user info, path, query and fragment are removed. Chat webhook URLs
// (Slack, Discord) carry their secret in the path, and net/http puts the
// request URL in its errors (*url.Error). The URL of a *url.Error in the
// chain is redacted even when it is not found by pattern; other URLs are
// found by pattern (scheme://...). RedactURL(nil) is "".
//
// It only protects URLs: an error that quotes a token or a header value
// some other way is not changed, so sinks must not put secrets in errors.
func RedactURL(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) && ue.URL != "" {
		msg = strings.ReplaceAll(msg, ue.URL, redactOne(ue.URL))
	}
	return urlPattern.ReplaceAllStringFunc(msg, redactOne)
}

// redactOne reduces one URL to scheme://host, marking a removed path or
// query with RedactedPath.
func redactOne(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		if i := strings.Index(raw, "://"); i >= 0 {
			return raw[:i+3] + "[redacted]"
		}
		return "[redacted]"
	}
	out := u.Scheme + "://" + u.Host
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		out += RedactedPath
	}
	return out
}
