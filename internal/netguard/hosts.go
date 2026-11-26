package netguard

// HostListed reports whether host is one of hosts, compared the way the
// limiter keys hosts: case-insensitive, without a trailing dot, so that
// "Broker.example.net." matches "broker.example.net". Every check of
// http.allow_http_hosts (webhook and mqtt sinks, the watch file, the
// dispatcher's secret destinations) uses it, so a host behaves the same
// everywhere.
func HostListed(hosts []string, host string) bool {
	h := normHost(host)
	if h == "" {
		return false
	}
	for _, e := range hosts {
		if normHost(e) == h {
			return true
		}
	}
	return false
}
