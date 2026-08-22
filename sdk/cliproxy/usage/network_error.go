package usage

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
)

// IsNetworkFailure reports whether err is a transport-level failure (DNS,
// dial, TLS, connection reset, or first-byte timeout) rather than an HTTP
// status returned by the upstream. Such failures are attributed to the
// network environment, not to the credential being used.
func IsNetworkFailure(err error) bool {
	if err == nil {
		return false
	}
	// Client cancellation is attributed to the client, not the network.
	if errors.Is(err, context.Canceled) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// http.Client.Do wraps transport errors in *url.Error; reaching here
	// means the attempt never produced an HTTP response.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	// ResponseHeaderTimeout and dial deadlines surface as deadline exceeded.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Upstream dropped the connection mid-response.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}
