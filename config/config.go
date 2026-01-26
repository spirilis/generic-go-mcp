package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents the application configuration
type Config struct {
	Server  ServerConfig   `yaml:"server"`
	Auth    *AuthConfig    `yaml:"auth,omitempty"`
	Logging *LoggingConfig `yaml:"logging,omitempty"`
}

// ServerConfig represents server-specific configuration
type ServerConfig struct {
	Mode string      `yaml:"mode"` // "stdio", "http", or "unix"
	HTTP *HTTPConfig `yaml:"http,omitempty"`
	Unix *UnixConfig `yaml:"unix,omitempty"`
}

// HTTPConfig represents HTTP server configuration
type HTTPConfig struct {
	Host string `yaml:"host"` // Default: "0.0.0.0"
	Port int    `yaml:"port"` // Default: 8080
}

// UnixConfig represents UNIX domain socket configuration
type UnixConfig struct {
	SocketPath string `yaml:"socket_path"` // Required
	Name       string `yaml:"name"`        // Required - exposed as /name resource
	FileMode   uint32 `yaml:"file_mode"`   // Optional, default 0660
}

// LoggingConfig represents logging configuration
type LoggingConfig struct {
	Level  string `yaml:"level"`  // "info", "debug", or "trace" (default: "info")
	Format string `yaml:"format"` // "text" or "json" (default: "text")
}

// AuthConfig represents OAuth authentication configuration
type AuthConfig struct {
	Enabled   bool              `yaml:"enabled"`           // Enable/disable auth (default: false)
	Issuer    string            `yaml:"issuer"`            // OAuth issuer URL (e.g., https://mcp.example.com)
	GitHub    GitHubConfig      `yaml:"github"`            // GitHub OAuth provider config
	Storage   StorageConfig     `yaml:"storage"`           // Token/session/client storage config
	Allowlist AllowlistConfig   `yaml:"allowlist"`         // Authorization allowlist
	Clients   []StaticClient    `yaml:"clients,omitempty"` // Pre-configured static clients
}

// GitHubConfig represents GitHub OAuth provider configuration
type GitHubConfig struct {
	ClientID         string `yaml:"clientId"`                   // GitHub OAuth App Client ID
	ClientSecret     string `yaml:"clientSecret"`               // GitHub OAuth App Client Secret
	ClientIDFile     string `yaml:"clientIdFile,omitempty"`     // Path to mounted secret file
	ClientSecretFile string `yaml:"clientSecretFile,omitempty"` // Path to mounted secret file
}

// StorageConfig represents storage paths for persistence
type StorageConfig struct {
	DBPath string `yaml:"dbPath"` // Path to BoltDB file (e.g., "/var/lib/go-mcp/oauth.db")
}

// AllowlistConfig defines who is authorized to use the MCP server
type AllowlistConfig struct {
	Users []string  `yaml:"users,omitempty"` // GitHub usernames
	Orgs  []string  `yaml:"orgs,omitempty"`  // GitHub organization names
	Teams []OrgTeam `yaml:"teams,omitempty"` // GitHub org/team pairs
}

// OrgTeam represents an organization and team pair
type OrgTeam struct {
	Org  string `yaml:"org"`
	Team string `yaml:"team"` // Team slug, not display name
}

// StaticClient represents a pre-configured OAuth client
type StaticClient struct {
	ClientID     string   `yaml:"clientId"`
	ClientSecret string   `yaml:"clientSecret"`
	Name         string   `yaml:"name"`
	RedirectURIs []string `yaml:"redirectUris"`
	Scopes       []string `yaml:"scopes,omitempty"`
}

// Load reads configuration from a YAML file
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	return LoadFromBytes(data)
}

// LoadFromBytes parses configuration from a YAML byte slice
func LoadFromBytes(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Apply defaults
	if cfg.Server.Mode == "" {
		cfg.Server.Mode = "stdio"
	}

	// Apply HTTP defaults
	if cfg.Server.Mode == "http" {
		if cfg.Server.HTTP == nil {
			cfg.Server.HTTP = &HTTPConfig{}
		}
		if cfg.Server.HTTP.Host == "" {
			cfg.Server.HTTP.Host = "0.0.0.0"
		}
		if cfg.Server.HTTP.Port == 0 {
			cfg.Server.HTTP.Port = 8080
		}
	}

	// Validate and apply UNIX defaults
	if cfg.Server.Mode == "unix" {
		if cfg.Server.Unix == nil {
			return nil, fmt.Errorf("unix configuration required when mode is 'unix'")
		}
		if cfg.Server.Unix.SocketPath == "" {
			return nil, fmt.Errorf("socket_path is required for unix mode")
		}
		if cfg.Server.Unix.Name == "" {
			return nil, fmt.Errorf("name is required for unix mode")
		}
		if cfg.Server.Unix.FileMode == 0 {
			cfg.Server.Unix.FileMode = 0660
		}
	}

	// Apply logging defaults
	if cfg.Logging == nil {
		cfg.Logging = &LoggingConfig{}
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "text"
	}

	return &cfg, nil
}

// LoadFromString parses configuration from a YAML string
func LoadFromString(yamlContent string) (*Config, error) {
	return LoadFromBytes([]byte(yamlContent))
}

// NewDefaultConfig creates a Config with all defaults applied
func NewDefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Mode: "stdio",
		},
		Logging: &LoggingConfig{
			Level:  "info",
			Format: "text",
		},
	}
}
