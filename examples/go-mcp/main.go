package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/spirilis/generic-go-mcp/auth"
	"github.com/spirilis/generic-go-mcp/config"
	"github.com/spirilis/generic-go-mcp/examples/tools"
	"github.com/spirilis/generic-go-mcp/logging"
	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"
)

// cliFlags holds all command-line flag values
type cliFlags struct {
	mode         string
	unixSocket   string
	unixName     string
	unixFileMode string
	httpHost     string
	httpPort     int
	logLevel     string
	logFormat    string
}

// applyCLIOverrides applies command-line flags to the configuration
func applyCLIOverrides(cfg *config.Config, flags cliFlags) {
	// Override mode
	if flags.mode != "" {
		cfg.Server.Mode = flags.mode
	}

	// Override unix settings
	if flags.unixSocket != "" || flags.unixName != "" || flags.unixFileMode != "" {
		if cfg.Server.Unix == nil {
			cfg.Server.Unix = &config.UnixConfig{}
		}
		if flags.unixSocket != "" {
			cfg.Server.Unix.SocketPath = flags.unixSocket
		}
		if flags.unixName != "" {
			cfg.Server.Unix.Name = flags.unixName
		}
		if flags.unixFileMode != "" {
			// Parse octal file mode
			var mode uint64
			_, err := fmt.Sscanf(flags.unixFileMode, "%o", &mode)
			if err == nil {
				cfg.Server.Unix.FileMode = uint32(mode)
			}
		}
	}

	// Override HTTP settings
	if flags.httpHost != "" || flags.httpPort != 0 {
		if cfg.Server.HTTP == nil {
			cfg.Server.HTTP = &config.HTTPConfig{}
		}
		if flags.httpHost != "" {
			cfg.Server.HTTP.Host = flags.httpHost
		}
		if flags.httpPort != 0 {
			cfg.Server.HTTP.Port = flags.httpPort
		}
	}

	// Override logging settings
	if flags.logLevel != "" {
		if cfg.Logging == nil {
			cfg.Logging = &config.LoggingConfig{}
		}
		cfg.Logging.Level = flags.logLevel
	}
	if flags.logFormat != "" {
		if cfg.Logging == nil {
			cfg.Logging = &config.LoggingConfig{}
		}
		cfg.Logging.Format = flags.logFormat
	}
}

// validateConfig validates the configuration
func validateConfig(cfg *config.Config) error {
	// Validate mode
	switch cfg.Server.Mode {
	case "stdio", "http", "unix":
		// Valid modes
	default:
		return fmt.Errorf("invalid mode '%s', must be stdio, http, or unix", cfg.Server.Mode)
	}

	// Validate unix mode requirements
	if cfg.Server.Mode == "unix" {
		if cfg.Server.Unix == nil {
			return fmt.Errorf("unix configuration required when mode is 'unix'")
		}
		if cfg.Server.Unix.SocketPath == "" {
			return fmt.Errorf("unix-socket is required for unix mode")
		}
		if cfg.Server.Unix.Name == "" {
			return fmt.Errorf("unix-name is required for unix mode")
		}
		// Apply default file mode if not set
		if cfg.Server.Unix.FileMode == 0 {
			cfg.Server.Unix.FileMode = 0660
		}
	}

	// Validate HTTP mode requirements
	if cfg.Server.Mode == "http" {
		if cfg.Server.HTTP == nil {
			cfg.Server.HTTP = &config.HTTPConfig{
				Host: "0.0.0.0",
				Port: 8080,
			}
		}
		if cfg.Server.HTTP.Host == "" {
			cfg.Server.HTTP.Host = "0.0.0.0"
		}
		if cfg.Server.HTTP.Port == 0 {
			cfg.Server.HTTP.Port = 8080
		}
	}

	return nil
}

func main() {
	// Define command-line flags
	configPath := flag.String("config", "", "Path to configuration file (optional)")
	mode := flag.String("mode", "", "Transport mode: stdio, http, unix")
	unixSocket := flag.String("unix-socket", "", "Unix socket path")
	unixName := flag.String("unix-name", "", "Server name for /name resource")
	unixFileMode := flag.String("unix-filemode", "", "Socket permissions (octal, e.g., 0660)")
	httpHost := flag.String("http-host", "", "HTTP bind address")
	httpPort := flag.Int("http-port", 0, "HTTP port")
	logLevel := flag.String("log-level", "", "Logging level")
	logFormat := flag.String("log-format", "", "Logging format")
	flag.Parse()

	// Load configuration
	var cfg *config.Config
	var err error

	if *configPath != "" {
		// Load from file
		cfg, err = config.Load(*configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Start with defaults
		cfg = config.NewDefaultConfig()
	}

	// Apply CLI overrides
	applyCLIOverrides(cfg, cliFlags{
		mode:         *mode,
		unixSocket:   *unixSocket,
		unixName:     *unixName,
		unixFileMode: *unixFileMode,
		httpHost:     *httpHost,
		httpPort:     *httpPort,
		logLevel:     *logLevel,
		logFormat:    *logFormat,
	})

	// Validate configuration
	if err := validateConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger early
	logging.Initialize(cfg.Logging)

	// Create tool registry and register tools
	registry := mcp.NewToolRegistry()
	registry.Register(tools.GetDateToolDefinition(), tools.DateTool)
	registry.Register(tools.GetFortuneToolDefinition(), tools.FortuneTool)

	// Create resource registry
	resourceRegistry := mcp.NewResourceRegistry()

	// Create MCP server
	server := mcp.NewServer(registry, resourceRegistry, &mcp.ServerConfig{
		Name:    "go-mcp-example",
		Version: "0.1.0",
	})

	// Initialize auth service if enabled
	var authService *auth.AuthService
	if cfg.Auth != nil && cfg.Auth.Enabled {
		var err error
		authService, err = auth.NewAuthService(cfg.Auth)
		if err != nil {
			logging.Error("Error initializing auth", "error", err)
			os.Exit(1)
		}
		defer authService.Close()
	}

	// Create and start transport based on config
	var trans transport.Transport
	switch cfg.Server.Mode {
	case "stdio":
		trans = transport.NewStdioTransport()
		logging.Info("Starting MCP server in stdio mode")
	case "http":
		trans = transport.NewHTTPTransport(transport.HTTPTransportConfig{
			Host:        cfg.Server.HTTP.Host,
			Port:        cfg.Server.HTTP.Port,
			AuthService: authService,
		})
		logging.Info("Starting MCP server in HTTP mode", "host", cfg.Server.HTTP.Host, "port", cfg.Server.HTTP.Port)
	case "unix":
		// Register /name resource
		resourceRegistry.Register(mcp.Resource{
			URI:         "/name",
			Name:        "Endpoint Name",
			Description: "The configured name of this MCP endpoint",
			MimeType:    "text/plain",
		}, func() (string, error) {
			return cfg.Server.Unix.Name, nil
		})

		// Register /pid resource
		resourceRegistry.Register(mcp.Resource{
			URI:         "/pid",
			Name:        "Process ID",
			Description: "PID of the MCP server process (send SIGINT or SIGTERM to stop)",
			MimeType:    "text/plain",
		}, func() (string, error) {
			return strconv.Itoa(os.Getpid()), nil
		})

		trans = transport.NewUnixTransport(transport.UnixTransportConfig{
			SocketPath: cfg.Server.Unix.SocketPath,
			FileMode:   os.FileMode(cfg.Server.Unix.FileMode),
		})
		logging.Info("Starting MCP server in UNIX socket mode",
			"socket", cfg.Server.Unix.SocketPath, "name", cfg.Server.Unix.Name)
	default:
		logging.Error("Unknown transport mode", "mode", cfg.Server.Mode)
		os.Exit(1)
	}

	// Start the transport
	if err := trans.Start(server); err != nil {
		logging.Error("Error starting transport", "error", err)
		os.Exit(1)
	}

	// Wait for interrupt signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	logging.Info("Shutting down gracefully")

	// Graceful shutdown
	if err := trans.Stop(); err != nil {
		logging.Error("Error stopping transport", "error", err)
		os.Exit(1)
	}

	logging.Info("Shutdown complete")
}
