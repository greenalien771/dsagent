package main

import (
	"net/http"
	"net/url"
	"strings"
)

// allowedHosts is filled at startup with the exact authorities this server can
// legitimately be reached on. Anything else is a DNS-rebinding or proxy probe.
var allowedHosts = map[string]bool{}

func addAllowedHost(authority string) {
	allowedHosts[strings.ToLower(authority)] = true
}

// hostAllowed rejects requests whose Host header is not one of our loopback
// bindings. This is what stops a malicious website from pointing a DNS name at
// 127.0.0.1 and driving this server (reading config, running tools).
func hostAllowed(r *http.Request) bool {
	h := strings.ToLower(strings.TrimSuffix(r.Host, "."))
	return allowedHosts[h]
}

// originAllowed gates state-changing requests. A browser always sends Origin for
// cross-origin POSTs; if one is present it must match exactly. Same-origin
// requests from our own page pass, and non-browser clients (no Origin) are not
// blocked by this check alone - the Host check covers those.
func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Scheme != "http" {
		return false
	}
	return allowedHosts[strings.ToLower(u.Host)]
}

// secFetchSite blocks cross-site browser contexts even when Origin is absent.
func secFetchSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

// contentSecurityPolicy keeps the UI's blast radius small: no remote script, no
// remote connect/img, no framing. Combined with redaction it means even a
// successful injection in a tool result has nowhere to send a secret.
// 'unsafe-inline' is required by the single-file UI; the policy still blocks
// every off-origin channel.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	// Nothing this server returns is cacheable, least of all config state.
	h.Set("Cache-Control", "no-store")
}

// postOnly narrows a mutating endpoint to the one verb it is meant to answer.
//
// It matters because guard's Origin check only covers non-GET requests: a plain
// cross-site GET (an <img> tag is enough) would otherwise reach /api/stop,
// /api/abort or /api/clear, which act on whatever method arrives. Browsers that
// send Sec-Fetch-Site are already stopped by guard, but that header is not
// universal, so the verb is checked here as well.
func postOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

// guard wraps a handler with the loopback-only policy:
//   - Host must be one of our bindings (anti DNS-rebinding)
//   - cross-site browser contexts are refused (anti CSRF)
//   - writes additionally require a matching Origin when one is sent
//   - every response carries a restrictive CSP and no-cache headers
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)

		if !hostAllowed(r) {
			http.Error(w, "forbidden: unexpected Host header", http.StatusForbidden)
			return
		}
		if !secFetchSite(r) {
			http.Error(w, "forbidden: cross-site request", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !originAllowed(r) {
			http.Error(w, "forbidden: unexpected Origin header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
