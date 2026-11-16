package config

import (
	"errors"
	"fmt"
	"strconv"
)

// Environment variables recognized by tabtenderd.
const (
	EnvConfigPath     = "TABTENDER_CONFIG_PATH"
	EnvDataDir        = "TABTENDER_DATA_DIR"
	EnvHost           = "TABTENDER_HOST"
	EnvPort           = "TABTENDER_PORT"
	EnvDefaultTTL     = "TABTENDER_DEFAULT_SESSION_TTL"
	EnvMaxTTL         = "TABTENDER_MAX_SESSION_TTL"
	EnvMaxSessions    = "TABTENDER_MAX_SESSIONS"
	EnvBrowserRuntime = "TABTENDER_BROWSER_RUNTIME"
	EnvChromePath     = "TABTENDER_CHROME_PATH"
	EnvTunnelProvider = "TABTENDER_TUNNEL_PROVIDER"
	EnvAPIKey         = "TABTENDER_API_KEY"
	EnvTunnelToken    = "TABTENDER_CLOUDFLARE_TUNNEL_TOKEN"
	EnvMgmtSocket     = "TABTENDER_MGMT_SOCKET"
	EnvSecretsBackend = "TABTENDER_SECRETS_BACKEND"
)

// Override describes one setting controlled by the environment.
type Override struct {
	Variable string `json:"variable"`
	Value    string `json:"value"`
}

// LookupEnv matches os.LookupEnv and exists so tests can inject environments.
type LookupEnv func(string) (string, bool)

type envBinding struct {
	key      string
	variable string
	apply    func(c *Config, raw string) error
}

func intSetter(dst func(c *Config) *int) func(*Config, string) error {
	return func(c *Config, raw string) error {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("expected an integer, got %q", raw)
		}
		*dst(c) = n
		return nil
	}
}

func stringSetter(dst func(c *Config) *string) func(*Config, string) error {
	return func(c *Config, raw string) error {
		*dst(c) = raw
		return nil
	}
}

var envBindings = []envBinding{
	{"server.host", EnvHost, stringSetter(func(c *Config) *string { return &c.Server.Host })},
	{"server.port", EnvPort, intSetter(func(c *Config) *int { return &c.Server.Port })},
	{"sessions.defaultTTL", EnvDefaultTTL, intSetter(func(c *Config) *int { return &c.Sessions.DefaultTTL })},
	{"sessions.maxTTL", EnvMaxTTL, intSetter(func(c *Config) *int { return &c.Sessions.MaxTTL })},
	{"sessions.maxConcurrent", EnvMaxSessions, intSetter(func(c *Config) *int { return &c.Sessions.MaxConcurrent })},
	{"browser.runtime", EnvBrowserRuntime, stringSetter(func(c *Config) *string { return &c.Browser.Runtime })},
	{"browser.executablePath", EnvChromePath, stringSetter(func(c *Config) *string { return &c.Browser.ExecutablePath })},
	{"tunnel.provider", EnvTunnelProvider, stringSetter(func(c *Config) *string { return &c.Tunnel.Provider })},
}

// applyEnv overlays environment-controlled settings onto c and returns the
// overrides that were applied, keyed by dotted setting name.
func applyEnv(c *Config, lookup LookupEnv) (map[string]Override, error) {
	overrides := map[string]Override{}
	var errs []error
	for _, b := range envBindings {
		raw, ok := lookup(b.variable)
		if !ok || raw == "" {
			continue
		}
		if err := b.apply(c, raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", b.variable, err))
			continue
		}
		overrides[b.key] = Override{Variable: b.variable, Value: raw}
	}
	return overrides, errors.Join(errs...)
}
