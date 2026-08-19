package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spirilis/generic-go-mcp/auth"
	"github.com/spirilis/generic-go-mcp/compat"
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

	// legacyCompat and legacyCompatSet implement "CLI flag overrides config file, but
	// only if the flag was actually passed" for a *bool flag: flag.Bool's own default
	// (false) is indistinguishable from an explicit -legacy-compat=false without tracking
	// whether flag.Visit saw it.
	legacyCompat    bool
	legacyCompatSet bool
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

	// Override legacy-compat, but only if -legacy-compat was actually passed: unlike
	// every other flag here, false is legacyCompat's own zero value, so "not passed" and
	// "explicitly disabled" are indistinguishable without legacyCompatSet.
	if flags.legacyCompatSet {
		if cfg.Server.LegacyCompat == nil {
			cfg.Server.LegacyCompat = &config.LegacyCompatConfig{}
		}
		cfg.Server.LegacyCompat.Enabled = flags.legacyCompat
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
	legacyCompat := flag.Bool("legacy-compat", false,
		"Serve MCP protocol revisions 2025-11-25 and earlier alongside 2026-07-28 (see the compat package)")
	flag.Parse()

	legacyCompatSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "legacy-compat" {
			legacyCompatSet = true
		}
	})

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
		mode:            *mode,
		unixSocket:      *unixSocket,
		unixName:        *unixName,
		unixFileMode:    *unixFileMode,
		httpHost:        *httpHost,
		httpPort:        *httpPort,
		logLevel:        *logLevel,
		logFormat:       *logFormat,
		legacyCompat:    *legacyCompat,
		legacyCompatSet: legacyCompatSet,
	})

	// Validate configuration
	if err := validateConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger early
	logging.Initialize(cfg.Logging.Level, cfg.Logging.Format, os.Stderr)

	// Create tool registry and register tools
	registry := mcp.NewToolRegistry()
	registry.Register(tools.GetDateToolDefinition(), tools.DateTool)
	registry.Register(tools.GetFortuneToolDefinition(), tools.FortuneTool)
	registry.Register(tools.GetConfirmToolDefinition(), tools.ConfirmTool)

	// Create resource registry
	resourceRegistry := mcp.NewResourceRegistry()

	const serverName, serverVersion = "go-mcp-example", "0.1.0"

	legacyCompatEnabled := cfg.Server.LegacyCompat != nil && cfg.Server.LegacyCompat.Enabled

	// Feed server/discover, the legacy initialize diagnostic, and -32022 from one list,
	// so a client is never told three different stories about what this server (overlay
	// included) actually speaks.
	advertisedVersions := []string{mcp.ProtocolVersion}
	if legacyCompatEnabled {
		advertisedVersions = append(advertisedVersions, compat.LegacyVersions...)
	}

	// Create MCP server
	server := mcp.NewServer(registry, resourceRegistry, &mcp.ServerConfig{
		Name:               serverName,
		Version:            serverVersion,
		AdvertisedVersions: advertisedVersions,
	})

	// Wrap in the legacy-compatibility overlay if enabled. handler is what actually gets
	// passed to the transport; legacySessions (nil unless compat is enabled) is what
	// drives the Streamable HTTP binding's legacy GET/DELETE surface.
	var handler transport.MessageHandler = server
	var legacySessions transport.LegacySessions
	if legacyCompatEnabled {
		sessionTTL := time.Duration(0) // compat.Overlay applies its own 30-minute default
		if cfg.Server.LegacyCompat.SessionTTL != "" {
			var err error
			sessionTTL, err = time.ParseDuration(cfg.Server.LegacyCompat.SessionTTL)
			if err != nil {
				logging.Error("Invalid server.legacy_compat.session_ttl", "value", cfg.Server.LegacyCompat.SessionTTL, "error", err)
				os.Exit(1)
			}
		}
		overlay := compat.New(server, compat.Config{
			SessionTTL: sessionTTL,
			ServerInfo: mcp.Implementation{Name: serverName, Version: serverVersion},
		})
		handler = overlay
		legacySessions = overlay.LegacySessions()
	}

	logging.Info("MCP protocol support",
		"modern", mcp.ProtocolVersion,
		"legacy_compat", legacyCompatEnabled,
		"advertised_versions", advertisedVersions)

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
		httpCfg := transport.HTTPTransportConfig{
			Host:           cfg.Server.HTTP.Host,
			Port:           cfg.Server.HTTP.Port,
			AllowedOrigins: cfg.Server.HTTP.AllowedOrigins,
			// nil (the default, when legacyCompatEnabled is false) means the legacy
			// GET/DELETE surface stays off — compat.Overlay.LegacySessions() already
			// guards against returning a typed nil, so this is always a genuinely nil
			// interface in that case, not the typed-nil trap AuthService below works
			// around.
			LegacySessions: legacySessions,
		}
		// Only set AuthService when auth is actually enabled: assigning a nil
		// *auth.AuthService unconditionally would store a typed nil in the
		// AuthProvider interface field, which is non-nil from the interface's
		// perspective and would make HTTPTransport treat auth as enabled.
		if authService != nil {
			httpCfg.AuthService = authService
		}
		trans = transport.NewHTTPTransport(httpCfg)
		logging.Info("Starting MCP server in HTTP mode", "host", cfg.Server.HTTP.Host, "port", cfg.Server.HTTP.Port)
	case "unix":
		// Register /name resource. Bare "/name" is not a valid absolute URI (no scheme);
		// this custom transport's resources use the mcp+unix:// scheme to identify them.
		resourceRegistry.Register(mcp.Resource{
			URI:         "mcp+unix:///name",
			Name:        "Endpoint Name",
			Description: "The configured name of this MCP endpoint",
			MimeType:    "text/plain",
		}, func(ctx context.Context) (mcp.ResourceContentResult, error) {
			return mcp.ResourceContentResult{Text: cfg.Server.Unix.Name}, nil
		})

		// Register /pid resource
		resourceRegistry.Register(mcp.Resource{
			URI:         "mcp+unix:///pid",
			Name:        "Process ID",
			Description: "PID of the MCP server process (send SIGINT or SIGTERM to stop)",
			MimeType:    "text/plain",
		}, func(ctx context.Context) (mcp.ResourceContentResult, error) {
			return mcp.ResourceContentResult{Text: strconv.Itoa(os.Getpid())}, nil
		})

		// Register /env/{name} as a resource *template*. The environment is an unbounded
		// keyspace as far as the protocol is concerned — there is no useful resources/list
		// of it — so the server advertises the shape via resources/templates/list and the
		// client expands it locally, then reads a concrete mcp+unix:///env/PATH.
		if err := resourceRegistry.RegisterTemplate(mcp.ResourceTemplate{
			URITemplate: "mcp+unix:///env/{name}",
			Name:        "Environment Variable",
			Title:       "Environment variable",
			Description: "The value of one environment variable of the server process",
			MimeType:    "text/plain",
		}, func(ctx context.Context, req *mcp.ResourceReadRequest) (mcp.ResourceContentResult, error) {
			value, ok := os.LookupEnv(req.Vars["name"])
			if !ok {
				// ErrResourceNotFound, so the client gets -32602 ("no such resource")
				// rather than -32603 ("this server is broken"). Returning empty content
				// instead would be indistinguishable from a variable set to "".
				return mcp.ResourceContentResult{}, fmt.Errorf(
					"environment variable %q is not set: %w", req.Vars["name"], mcp.ErrResourceNotFound)
			}
			return mcp.ResourceContentResult{Text: value}, nil
		}); err != nil {
			logging.Error("Failed to register env resource template", "error", err)
			os.Exit(1)
		}

		// Completion is optional and entirely the embedder's business: the library owns the
		// completion/complete wire format, this callback owns what is actually in the
		// keyspace. Registering it is what makes the server declare the completions
		// capability at all.
		if err := resourceRegistry.SetTemplateCompleter("mcp+unix:///env/{name}", mcp.CompletionFunc(
			func(ctx context.Context, req *mcp.CompletionRequest) (mcp.CompletionResult, error) {
				var names []string
				for _, entry := range os.Environ() {
					name, _, _ := strings.Cut(entry, "=")
					if strings.HasPrefix(name, req.Value) {
						names = append(names, name)
					}
				}
				sort.Strings(names)
				total := len(names)
				return mcp.CompletionResult{Values: names, Total: &total}, nil
			})); err != nil {
			logging.Error("Failed to register env completion provider", "error", err)
			os.Exit(1)
		}

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
	if err := trans.Start(handler); err != nil {
		logging.Error("Error starting transport", "error", err)
		os.Exit(1)
	}

	// Wait for either an interrupt signal or the transport's own run ending. Only stdio has
	// the latter: its input reaching EOF means the client that launched us is done, which is
	// the portable shutdown signal for a stdio server. For http/unix the assertion fails,
	// transDone stays nil, and a receive on a nil channel blocks forever — so this select
	// degrades to waiting on the signal alone, exactly as before.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	var transDone <-chan struct{}
	if d, ok := trans.(transport.DoneNotifier); ok {
		transDone = d.Done()
	}

	select {
	case <-sigCh:
		logging.Info("Shutting down gracefully", "reason", "signal")
	case <-transDone:
		logging.Info("Shutting down gracefully", "reason", "input closed")
	}

	// Graceful shutdown
	if err := trans.Stop(); err != nil {
		logging.Error("Error stopping transport", "error", err)
		os.Exit(1)
	}

	// A stdio run that ended because reading its input failed is not a clean exit; report it
	// as one and a supervising client has no way to tell a crash from a normal disconnect.
	if stdio, ok := trans.(*transport.StdioTransport); ok {
		select {
		case <-stdio.Done():
			if err := stdio.Err(); err != nil {
				logging.Error("stdio input failed", "error", err)
				os.Exit(1)
			}
		default:
		}
	}

	logging.Info("Shutdown complete")
}
