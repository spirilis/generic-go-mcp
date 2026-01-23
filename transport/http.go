package transport

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/spirilis/generic-go-mcp/auth"
	"github.com/spirilis/generic-go-mcp/logging"
)

// responseRecorder wraps http.ResponseWriter to capture response details
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	size       int
	body       *bytes.Buffer
}

func newResponseRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{
		ResponseWriter: w,
		statusCode:     http.StatusOK, // Default status
		body:           &bytes.Buffer{},
	}
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	// Capture body if trace logging is enabled
	if logging.IsTraceEnabled() {
		r.body.Write(b)
	}
	n, err := r.ResponseWriter.Write(b)
	r.size += n
	return n, err
}

// Session represents an SSE client session
type Session struct {
	ID       string
	Response chan []byte
	Done     chan struct{}
	User     *auth.User  // Authenticated user (if auth enabled)
	ClientID string      // OAuth client ID (if auth enabled)
}

// SessionManager manages active SSE sessions
type SessionManager struct {
	sessions sync.Map // map[string]*Session
}

// NewSessionManager creates a new session manager
func NewSessionManager() *SessionManager {
	return &SessionManager{}
}

// CreateSession creates a new session with a unique ID
func (sm *SessionManager) CreateSession() *Session {
	session := &Session{
		ID:       generateUUID(),
		Response: make(chan []byte, 10),
		Done:     make(chan struct{}),
	}
	sm.sessions.Store(session.ID, session)
	return session
}

// GetSession retrieves a session by ID
func (sm *SessionManager) GetSession(id string) (*Session, bool) {
	val, ok := sm.sessions.Load(id)
	if !ok {
		return nil, false
	}
	return val.(*Session), true
}

// RemoveSession removes a session by ID
func (sm *SessionManager) RemoveSession(id string) {
	if val, ok := sm.sessions.Load(id); ok {
		session := val.(*Session)
		close(session.Done)
		sm.sessions.Delete(id)
	}
}

// HTTPTransportConfig holds configuration for HTTP transport
type HTTPTransportConfig struct {
	Host        string
	Port        int
	AuthService *auth.AuthService // Optional auth service
}

// HTTPTransport implements Transport using HTTP/SSE
type HTTPTransport struct {
	config         HTTPTransportConfig
	sessionManager *SessionManager
	handler        MessageHandler
	server         *http.Server
	stopCh         chan struct{}
	wg             sync.WaitGroup
	authService    *auth.AuthService
}

// NewHTTPTransport creates a new HTTP transport
func NewHTTPTransport(config HTTPTransportConfig) *HTTPTransport {
	// Set defaults
	if config.Host == "" {
		config.Host = "0.0.0.0"
	}
	if config.Port == 0 {
		config.Port = 8080
	}

	return &HTTPTransport{
		config:         config,
		sessionManager: NewSessionManager(),
		stopCh:         make(chan struct{}),
		authService:    config.AuthService,
	}
}

// Start begins the HTTP server
func (t *HTTPTransport) Start(handler MessageHandler) error {
	t.handler = handler

	mux := http.NewServeMux()

	// Register auth endpoints if auth is enabled
	if t.authService != nil {
		t.authService.RegisterRoutes(mux)
		t.authService.RegisterAdminRoutes(mux)

		// Wrap /mcp with auth middleware
		mux.Handle("/mcp", t.authService.Middleware(http.HandlerFunc(t.handleMCP)))
		logging.Info("OAuth authentication enabled")
	} else {
		// No auth - direct handler
		mux.HandleFunc("/mcp", t.handleMCP)
	}

	t.server = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", t.config.Host, t.config.Port),
		Handler: mux,
	}

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		logging.Info("HTTP server listening", "addr", t.server.Addr, "transport", "Streamable HTTP")
		if err := t.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logging.Error("HTTP server error", "error", err)
		}
	}()

	return nil
}

// Stop gracefully stops the HTTP server
func (t *HTTPTransport) Stop() error {
	close(t.stopCh)

	if t.server != nil {
		if err := t.server.Close(); err != nil {
			return err
		}
	}

	t.wg.Wait()
	return nil
}

// handleMCP handles the /mcp endpoint for Streamable HTTP transport
func (t *HTTPTransport) handleMCP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Wrap response writer to capture details
	recorder := newResponseRecorder(w)

	// Enable CORS
	recorder.Header().Set("Access-Control-Allow-Origin", "*")
	recorder.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	recorder.Header().Set("Access-Control-Allow-Headers", "Content-Type, Mcp-Session-Id, Accept")

	if r.Method == http.MethodOptions {
		recorder.WriteHeader(http.StatusOK)
		return
	}

	// Trace: Log request details
	if logging.IsTraceEnabled() {
		sanitizedHeaders := logging.SanitizeHeaders(r.Header)
		logging.Trace("HTTP request received",
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"headers", sanitizedHeaders)
	}

	switch r.Method {
	case http.MethodPost:
		t.handlePost(recorder, r)
	case http.MethodGet:
		t.handleGet(recorder, r)
	case http.MethodDelete:
		t.handleDelete(recorder, r)
	default:
		http.Error(recorder, "Method not allowed", http.StatusMethodNotAllowed)
	}

	// Log request completion
	duration := time.Since(start)
	sessionID := r.Header.Get("Mcp-Session-Id")

	if logging.IsDebugEnabled() {
		logArgs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.statusCode,
			"size", recorder.size,
			"duration_ms", duration.Milliseconds(),
			"remote_addr", r.RemoteAddr,
		}

		if sessionID != "" {
			logArgs = append(logArgs, "session_id", sessionID)
		}

		// Add user info if available
		if user := auth.GetUserFromContext(r.Context()); user != nil {
			logArgs = append(logArgs, "user_id", user.ID, "github_login", user.GitHubLogin)
		}

		logging.Debug("HTTP request completed", logArgs...)
	}

	// Trace: Log response body
	if logging.IsTraceEnabled() && recorder.body.Len() > 0 {
		logging.Trace("HTTP response body", "body", recorder.body.String())
	}
}

// handlePost handles POST requests (client → server messages)
func (t *HTTPTransport) handlePost(w http.ResponseWriter, r *http.Request) {
	// Read JSON-RPC request
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Trace: Log request body
	if logging.IsTraceEnabled() {
		logging.Trace("HTTP POST request body", "body", string(body))
	}

	// Parse request to check if it's an initialize request
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	method, _ := req["method"].(string)

	if method == "initialize" {
		var session *Session
		sessionID := r.Header.Get("Mcp-Session-Id")

		// Check if session already exists (SSE-first pattern)
		if sessionID != "" {
			existingSession, ok := t.sessionManager.GetSession(sessionID)
			if ok {
				session = existingSession
				logging.Debug("Using existing session for initialize", "session_id", session.ID)
			}
		}

		// Create new session if none exists (POST-first pattern)
		if session == nil {
			session = t.sessionManager.CreateSession()

			// Attach authenticated user to session if auth is enabled
			if t.authService != nil {
				user := auth.GetUserFromContext(r.Context())
				if user != nil {
					session.User = user
				}
				token := auth.GetAccessTokenFromContext(r.Context())
				if token != nil {
					session.ClientID = token.ClientID
				}
			}

			logArgs := []any{"session_id", session.ID}
			if session.User != nil {
				logArgs = append(logArgs, "user_id", session.User.ID, "github_login", session.User.GitHubLogin)
			}
			logging.Debug("Session created via POST", logArgs...)
		}

		// Process request
		response := t.handler.HandleMessage(body)

		// Return response with session ID header
		w.Header().Set("Mcp-Session-Id", session.ID)
		w.Header().Set("Content-Type", "application/json")
		w.Write(response)
		return
	}

	// For other requests, validate session
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		http.Error(w, "Missing Mcp-Session-Id header", http.StatusBadRequest)
		return
	}

	_, ok := t.sessionManager.GetSession(sessionID)
	if !ok {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	// Process request and return response
	response := t.handler.HandleMessage(body)
	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}

// handleGet handles GET requests (server → client notifications via SSE)
func (t *HTTPTransport) handleGet(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	var session *Session
	var isNewSession bool

	if sessionID == "" {
		// No session ID - create new session (SSE-first pattern for Claude Code)
		session = t.sessionManager.CreateSession()
		isNewSession = true

		// Attach authenticated user to session if auth is enabled
		if t.authService != nil {
			user := auth.GetUserFromContext(r.Context())
			if user != nil {
				session.User = user
			}
			token := auth.GetAccessTokenFromContext(r.Context())
			if token != nil {
				session.ClientID = token.ClientID
			}
		}

		logArgs := []any{"session_id", session.ID}
		if session.User != nil {
			logArgs = append(logArgs, "user_id", session.User.ID, "github_login", session.User.GitHubLogin)
		}
		logging.Debug("Session created via SSE", logArgs...)
	} else {
		// Existing session - validate
		var ok bool
		session, ok = t.sessionManager.GetSession(sessionID)
		if !ok {
			http.Error(w, "Session not found", http.StatusNotFound)
			return
		}
		logging.Debug("SSE connection established", "session_id", sessionID, "remote_addr", r.RemoteAddr)
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Mcp-Session-Id", session.ID) // Always return session ID

	// Flush headers immediately
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// If new session, send endpoint event with session info
	if isNewSession {
		endpointEvent := fmt.Sprintf("event: endpoint\ndata: /mcp?sessionId=%s\n\n", session.ID)
		fmt.Fprint(w, endpointEvent)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	// Keep connection alive and send server-initiated messages
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			logging.Debug("SSE connection closed by client", "session_id", session.ID)
			return
		case <-session.Done:
			logging.Debug("SSE connection closed (session ended)", "session_id", session.ID)
			return
		case <-t.stopCh:
			return
		case <-ticker.C:
			// Send keep-alive comment
			fmt.Fprintf(w, ": ping\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case msg := <-session.Response:
			// Send server-initiated message as SSE event
			if logging.IsTraceEnabled() {
				logging.Trace("SSE sending message", "session_id", session.ID, "message", string(msg))
			}
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
}

// handleDelete handles DELETE requests (session cleanup)
func (t *HTTPTransport) handleDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		http.Error(w, "Missing Mcp-Session-Id header", http.StatusBadRequest)
		return
	}

	logging.Debug("Session deleted", "session_id", sessionID)
	t.sessionManager.RemoveSession(sessionID)
	w.WriteHeader(http.StatusOK)
}

// generateUUID generates a UUID v4
func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)

	// Set version (4) and variant bits
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return hex.EncodeToString(b[:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:])
}
