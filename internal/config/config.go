// Package config loads, validates, and persists Tabtender configuration.
//
// Resolution order is: environment variables > configuration file > built-in
// defaults. The file layer and the effective (resolved) layer are kept apart so
// that saving configuration never writes environment-controlled values back to
// disk.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Tunnel provider kinds understood by the built-in tunnel factory.
const (
	TunnelNone            = "none"
	TunnelCloudflareQuick = "cloudflare-quick"
	TunnelCloudflareNamed = "cloudflare-named"
)

// ErrEnvironmentControlled marks attempts to change a setting that is pinned
// by an environment variable.
var ErrEnvironmentControlled = errors.New("setting is controlled by the environment")

// AutoPath asks the service to discover an executable on its own.
const AutoPath = "auto"

// Config is the complete Tabtender configuration document.
type Config struct {
	Server   Server   `yaml:"server" json:"server"`
	Sessions Sessions `yaml:"sessions" json:"sessions"`
	Browser  Browser  `yaml:"browser" json:"browser"`
	Tunnel   Tunnel   `yaml:"tunnel" json:"tunnel"`
	App      App      `yaml:"app" json:"app"`
}

// Server configures the HTTP listener.
type Server struct {
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
	// PublicURL optionally pins the externally visible base URL (for example
	// when running behind a reverse proxy that terminates TLS or mTLS).
	PublicURL string `yaml:"publicUrl,omitempty" json:"publicUrl"`
}

// Sessions configures session limits. TTLs are expressed in seconds.
type Sessions struct {
	DefaultTTL    int `yaml:"defaultTTL" json:"defaultTTL"`
	MaxTTL        int `yaml:"maxTTL" json:"maxTTL"`
	MaxConcurrent int `yaml:"maxConcurrent" json:"maxConcurrent"`
}

// Browser configures the browser runtime adapter.
type Browser struct {
	Runtime        string   `yaml:"runtime" json:"runtime"`
	ExecutablePath string   `yaml:"executablePath" json:"executablePath"`
	Headless       bool     `yaml:"headless" json:"headless"`
	ExtraArgs      []string `yaml:"extraArgs,omitempty" json:"extraArgs"`
}

// Tunnel configures the tunnel provider.
type Tunnel struct {
	Provider   string     `yaml:"provider" json:"provider"`
	Cloudflare Cloudflare `yaml:"cloudflare,omitempty" json:"cloudflare"`
}

// Cloudflare holds settings shared by the Cloudflare tunnel providers.
type Cloudflare struct {
	// ExecutablePath locates cloudflared; "auto" lets Tabtender find or
	// download a pinned release.
	ExecutablePath string `yaml:"executablePath,omitempty" json:"executablePath"`
	// Hostname is the public hostname routed to this service by a named
	// tunnel (for example "browser.example.com").
	Hostname string `yaml:"hostname,omitempty" json:"hostname"`
}

// App holds settings for OS integration.
type App struct {
	StartOnLogin bool `yaml:"startOnLogin" json:"startOnLogin"`
	PreventSleep bool `yaml:"preventSleep" json:"preventSleep"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Server:   Server{Host: "127.0.0.1", Port: 3000},
		Sessions: Sessions{DefaultTTL: 3600, MaxTTL: 14400, MaxConcurrent: 8},
		Browser:  Browser{Runtime: "chrome", ExecutablePath: AutoPath, Headless: false},
		Tunnel: Tunnel{
			Provider:   TunnelNone,
			Cloudflare: Cloudflare{ExecutablePath: AutoPath},
		},
		App: App{StartOnLogin: true, PreventSleep: true},
	}
}

// normalize fills values that may be legitimately omitted from a document.
func (c *Config) normalize() {
	if c.Browser.ExecutablePath == "" {
		c.Browser.ExecutablePath = AutoPath
	}
	if c.Tunnel.Provider == "" {
		c.Tunnel.Provider = TunnelNone
	}
	if c.Tunnel.Cloudflare.ExecutablePath == "" {
		c.Tunnel.Cloudflare.ExecutablePath = AutoPath
	}
	if c.Browser.ExtraArgs == nil {
		c.Browser.ExtraArgs = []string{}
	}
	c.Server.PublicURL = strings.TrimRight(c.Server.PublicURL, "/")
}

// Validate reports every problem found in the configuration.
func (c Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Server.Host == "" {
		add("server.host must not be empty")
	} else if net.ParseIP(c.Server.Host) == nil && c.Server.Host != "localhost" {
		add("server.host must be an IP address or \"localhost\", got %q", c.Server.Host)
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		add("server.port must be between 1 and 65535, got %d", c.Server.Port)
	}
	if c.Server.PublicURL != "" {
		u, err := url.Parse(c.Server.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("server.publicUrl must be an http(s) URL, got %q", c.Server.PublicURL)
		}
	}

	if c.Sessions.MaxTTL < 1 {
		add("sessions.maxTTL must be at least 1 second, got %d", c.Sessions.MaxTTL)
	}
	if c.Sessions.DefaultTTL < 1 {
		add("sessions.defaultTTL must be at least 1 second, got %d", c.Sessions.DefaultTTL)
	} else if c.Sessions.MaxTTL >= 1 && c.Sessions.DefaultTTL > c.Sessions.MaxTTL {
		add("sessions.defaultTTL (%d) must not exceed sessions.maxTTL (%d)", c.Sessions.DefaultTTL, c.Sessions.MaxTTL)
	}
	if c.Sessions.MaxConcurrent < 1 {
		add("sessions.maxConcurrent must be at least 1, got %d", c.Sessions.MaxConcurrent)
	}

	if c.Browser.Runtime == "" {
		add("browser.runtime must not be empty")
	}

	// Which providers exist is decided by the tunnel factory, not here, so
	// new providers need no configuration changes.
	if c.Tunnel.Provider == "" {
		add("tunnel.provider must not be empty")
	}
	if h := c.Tunnel.Cloudflare.Hostname; h != "" && strings.ContainsAny(h, "/: ") {
		add("tunnel.cloudflare.hostname must be a bare hostname, got %q", h)
	}
	return errors.Join(errs...)
}

// RestartRequiredKeys lists settings that only take effect after the service
// restarts. Every other setting is applied live.
var RestartRequiredKeys = []string{"server.host", "server.port"}

// PendingRestart returns the restart-required keys whose value differs
// between the running configuration and a newly resolved one.
func PendingRestart(running, next Config) []string {
	pending := []string{}
	if running.Server.Host != next.Server.Host {
		pending = append(pending, "server.host")
	}
	if running.Server.Port != next.Server.Port {
		pending = append(pending, "server.port")
	}
	return pending
}
