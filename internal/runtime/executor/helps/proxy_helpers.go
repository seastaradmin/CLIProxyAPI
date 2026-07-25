package helps

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// NewProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyURL if configured (highest priority)
// 2. Use cfg.ProxyURL if auth proxy is not configured
// 3. Use RoundTripper from context if neither are configured
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func NewProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}

	// Priority 1: Use auth.ProxyURL if configured
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}

	// Priority 2: Use cfg.ProxyURL if auth proxy is not configured
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	headerTimeout := responseHeaderTimeout(cfg, auth)

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		transport := buildProxyTransport(proxyURL)
		if transport != nil {
			applyResponseHeaderTimeout(transport, headerTimeout)
			httpClient.Transport = transport
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyutil.Redact(proxyURL))
	}

	// Priority 3: Use RoundTripper from context (typically from RoundTripperFor)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = wrapRoundTripperHeaderTimeout(rt, headerTimeout)
		return httpClient
	}

	// Default transport with first-byte timeout.
	if headerTimeout > 0 {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		applyResponseHeaderTimeout(transport, headerTimeout)
		httpClient.Transport = transport
	}

	return httpClient
}

func responseHeaderTimeoutFromConfig(cfg *config.Config) time.Duration {
	if cfg == nil {
		return config.ResolveResponseHeaderTimeout(0)
	}
	return config.ResolveResponseHeaderTimeout(cfg.ResponseHeaderTimeoutSeconds)
}

// responseHeaderTimeout resolves per-auth override first, then global config.
// Auth attribute "response_header_timeout_seconds": 0/missing inherits global;
// negative disables; positive uses that many seconds.
func responseHeaderTimeout(cfg *config.Config, auth *cliproxyauth.Auth) time.Duration {
	if auth != nil && auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["response_header_timeout_seconds"]); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				return config.ResolveResponseHeaderTimeout(n)
			}
		}
	}
	return responseHeaderTimeoutFromConfig(cfg)
}

func applyResponseHeaderTimeout(transport *http.Transport, headerTimeout time.Duration) {
	if transport == nil || headerTimeout <= 0 {
		return
	}
	transport.ResponseHeaderTimeout = headerTimeout
}

// wrapRoundTripperHeaderTimeout applies ResponseHeaderTimeout when the base
// round tripper is (or wraps) an *http.Transport. Other transports are returned as-is.
func wrapRoundTripperHeaderTimeout(rt http.RoundTripper, headerTimeout time.Duration) http.RoundTripper {
	if rt == nil || headerTimeout <= 0 {
		return rt
	}
	if transport, ok := rt.(*http.Transport); ok && transport != nil {
		cloned := transport.Clone()
		applyResponseHeaderTimeout(cloned, headerTimeout)
		return cloned
	}
	return rt
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string) *http.Transport {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	return transport
}
