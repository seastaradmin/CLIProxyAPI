package usage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"testing"
)

func TestIsNetworkFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"client cancel", context.Canceled, false},
		{"wrapped client cancel", fmt.Errorf("request failed: %w", context.Canceled), false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"dns error", &net.DNSError{Err: "no such host", Name: "opencode.ai"}, true},
		{"dial op error", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"url error", &url.Error{Op: "Get", URL: "https://upstream", Err: context.DeadlineExceeded}, true},
		{"wrapped url error", fmt.Errorf("do request: %w", &url.Error{Op: "Post", Err: io.ErrUnexpectedEOF}), true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
	}
	for _, tc := range cases {
		if got := IsNetworkFailure(tc.err); got != tc.want {
			t.Fatalf("%s: IsNetworkFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}
