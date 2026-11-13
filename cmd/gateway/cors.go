package main

import (
	"net/http"
	"sync/atomic"
)

// dynamicCORS keeps the allowed-origin set in an atomic value so SIGHUP and
// the admin API can change CORS policy without rebuilding the HTTP server.
// It mirrors proxy.CORS semantics but reads the set per request.
type dynamicCORS struct {
	allowed atomic.Value // map[string]bool (immutable after Store)
}

func newDynamicCORS(origins []string) *dynamicCORS {
	c := &dynamicCORS{}
	c.Set(origins)
	return c
}

// Set atomically replaces the allowed origins.
func (c *dynamicCORS) Set(origins []string) {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	c.allowed.Store(allowed)
}

func (c *dynamicCORS) list() map[string]bool {
	v, _ := c.allowed.Load().(map[string]bool)
	return v
}

// Wrap returns the middleware. The wrapper is installed unconditionally so
// enabling CORS later through a reload takes effect immediately.
func (c *dynamicCORS) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := c.list()
		if len(allowed) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		origin := r.Header.Get("Origin")
		if allowed["*"] || allowed[origin] {
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Origin", origin)
			if origin == "" && allowed["*"] {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID")
		}
		if r.Method == http.MethodOptions {
			if origin != "" && !allowed["*"] && !allowed[origin] {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
