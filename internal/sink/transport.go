package sink

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

// TransportClass describes an HTTP request that got no response with a
// fixed class ("timeout", "connection refused", "TLS handshake failed",
// ...) that never holds the dialled address, port or URL, for sink errors
// and last_error.
func TransportClass(err error) string {
	var (
		dnsErr  *net.DNSError
		ne      net.Error
		certErr *tls.CertificateVerificationError
		recErr  tls.RecordHeaderError
		alert   tls.AlertError
		unknown x509.UnknownAuthorityError
		badHost x509.HostnameError
		invalid x509.CertificateInvalidError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dnsErr):
		return "DNS lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection reset"
	case errors.As(err, &certErr), errors.As(err, &recErr), errors.As(err, &alert),
		errors.As(err, &unknown), errors.As(err, &badHost), errors.As(err, &invalid):
		return "TLS handshake failed"
	case strings.Contains(err.Error(), "netguard: resolve "):
		return "DNS lookup failed"
	default:
		return "request failed"
	}
}
