package badge

import "net/http"

// WithHTTPClient injects an http.Client (tests use this to point at a stub
// server / assert timeouts without a live model).
func WithHTTPClient(c *http.Client) Option { return func(g *Generator) { g.client = c } }
