// Package platform holds cross-cutting infrastructure that the domain packages
// build on but that knows nothing about any domain: configuration loading,
// structured logging, the HTTP server run/shutdown lifecycle, and build/health
// metadata. Per the architecture's layering rule, platform never imports a
// domain package.
package platform
