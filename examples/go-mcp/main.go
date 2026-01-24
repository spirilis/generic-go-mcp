package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spirilis/generic-go-mcp/auth"
	"github.com/spirilis/generic-go-mcp/config"
	"github.com/spirilis/generic-go-mcp/examples/tools"
	"github.com/spirilis/generic-go-mcp/logging"
	"github.com/spirilis/generic-go-mcp/mcp"
	"github.com/spirilis/generic-go-mcp/transport"
)

func main() {
	// Parse command-line flags
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger early
	logging.Initialize(cfg.Logging)

	// Create tool registry and register tools
	registry := mcp.NewToolRegistry()
	registry.Register(tools.GetDateToolDefinition(), tools.DateTool)
	registry.Register(tools.GetFortuneToolDefinition(), tools.FortuneTool)

	// Create MCP server
	server := mcp.NewServer(registry, &mcp.ServerConfig{
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
