package platform

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

// envPrefix namespaces every environment variable so that platform config never
// collides with unrelated variables in a shared environment (e.g. a container
// host). ADDR is therefore read from TYPEMORE_ADDR, and so on.
const envPrefix = "TYPEMORE_"

// Config is the fully-resolved server configuration. It is populated once at
// startup from environment variables (see LoadConfig) and then treated as
// immutable — pass it by value to consumers.
//
// Defaults are chosen so that `go run ./cmd/server` works with no environment
// set at all; every field can be overridden via its TYPEMORE_-prefixed variable.
type Config struct {
	// Addr is the TCP address the HTTP server listens on, in Go's host:port
	// form. An empty host (":8080") binds all interfaces.
	Addr string `env:"ADDR" envDefault:":8080"`

	// LogLevel is the minimum slog level to emit: debug, info, warn, or error.
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	// LogFormat selects the slog handler: "json" (default, machine-readable) or
	// "text" (human-readable, handy during local development).
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`

	// ShutdownTimeout bounds how long graceful shutdown waits for in-flight
	// work to finish before the process exits anyway.
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`

	// ReadHeaderTimeout bounds how long the server waits for a request's
	// headers. It is deliberately the only HTTP timeout set: whole-request
	// Read/Write timeouts would kill long-lived WebSocket connections.
	ReadHeaderTimeout time.Duration `env:"READ_HEADER_TIMEOUT" envDefault:"5s"`

	// AllowedOrigins is the WebSocket Origin allow-list (browser CSRF defense).
	// Values are matched against the request Origin host with wildcard support
	// (e.g. "*.typemore.gg"). When empty, all origins are allowed — convenient
	// for local development but you SHOULD set it in production.
	AllowedOrigins []string `env:"ALLOWED_ORIGINS" envSeparator:","`
}

// LoadConfig reads Config from the process environment. It returns an error if a
// value is present but malformed (e.g. a non-parseable duration), so a
// misconfigured deployment fails fast at startup instead of misbehaving later.
func LoadConfig() (Config, error) {
	var c Config
	if err := env.ParseWithOptions(&c, env.Options{Prefix: envPrefix}); err != nil {
		return Config{}, fmt.Errorf("parse env config: %w", err)
	}
	return c, nil
}
