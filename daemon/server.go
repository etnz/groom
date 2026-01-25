package daemon

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Config holds the configuration parameters for the Daemon Server.
type Config struct {
	ListenAddr      string
	Version         string
	SelfPackageName string
	PoolDir         string
	InstalledDir    string
}

// Server represents the daemon service agent.
type Server struct {
	cfg             Config
	httpServer      *http.Server
	stopAdvertising func()
}

// New creates a new Server instance with the provided configuration.
func New(cfg Config) *Server {
	return &Server{
		cfg: cfg,
	}
}

// Start initializes resources and starts the background services (HTTP, mDNS).
// It is non-blocking.
func (s *Server) Start() {
	log.Printf("🎩 Groom Service started on %s", s.cfg.ListenAddr)

	// Ensure directories exist
	os.MkdirAll(s.cfg.PoolDir, 0755)
	os.MkdirAll(s.cfg.InstalledDir, 0755)

	// Extract port for mDNS
	_, portStr, err := net.SplitHostPort(s.cfg.ListenAddr)
	if err != nil {
		if strings.HasPrefix(s.cfg.ListenAddr, ":") {
			portStr = s.cfg.ListenAddr[1:]
		} else {
			portStr = "8080"
		}
	}
	port, _ := strconv.Atoi(portStr)

	// Start mDNS advertising
	s.stopAdvertising = s.startAdvertisingOp(port)

	// Setup HTTP Server
	mux := http.NewServeMux()
	s.registerHandlers(mux)

	s.httpServer = &http.Server{
		Addr:    s.cfg.ListenAddr,
		Handler: mux,
	}

	// Start HTTP Server in a goroutine
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()
}

// Stop gracefully shuts down the server and its background processes.
func (s *Server) Stop(ctx context.Context) {
	log.Println("👋 Shutdown signal received.")

	if s.stopAdvertising != nil {
		s.stopAdvertising()
	}

	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(ctx); err != nil {
			log.Printf("HTTP shutdown error: %v", err)
		}
	}
	log.Println("🛑 Groom stopped.")
}
