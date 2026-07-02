package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"q8s/internal/store"
	"q8s/internal/systemd"
)

// Config holds server configuration.
type Config struct {
	Store      *store.Store
	CACert     []byte
	CertPEM    []byte
	KeyPEM     []byte
	QuadletDir  string // Podman quadlet dir for .container/.volume/.network files
	SystemdDir  string // systemd user unit dir for .timer/.service files
	ConfigDir   string // base dir for ConfigMap files: {ConfigDir}/{ns}/{cm-name}/
	Mode       systemd.Mode
	Manager    *systemd.Manager
}

// Server is the k8s-compatible API server.
type Server struct {
	httpServer *http.Server
	config     Config
	mux        *http.ServeMux
	auth       *AuthMiddleware
}

// New creates a new Server.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if len(cfg.CertPEM) == 0 || len(cfg.KeyPEM) == 0 {
		return nil, fmt.Errorf("TLS certificate and key are required")
	}

	s := &Server{
		config: cfg,
		mux:    http.NewServeMux(),
		auth:   NewAuthMiddleware(cfg.CACert),
	}

	s.setupRoutes()

	cert, err := tls.X509KeyPair(cfg.CertPEM, cfg.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS cert: %w", err)
	}

	s.httpServer = &http.Server{
		Handler: s,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    s.auth.caCertPool,
			MinVersion:   tls.VersionTLS12,
		},
		ReadHeaderTimeout: 10 * time.Second,
	}

	return s, nil
}

// StartTLS starts the server with TLS on the given listeners.
func (s *Server) StartTLS(listeners []net.Listener) error {
	if len(listeners) == 0 {
		return fmt.Errorf("no listeners provided")
	}

	tlsListener := tls.NewListener(listeners[0], s.httpServer.TLSConfig)
	go func() {
		if err := s.httpServer.Serve(tlsListener); err != http.ErrServerClosed {
			fmt.Printf("HTTP server error: %v\n", err)
		}
	}()

	if len(listeners) > 1 {
		unixSrv := &http.Server{
			Handler:   s,
			TLSConfig: s.httpServer.TLSConfig,
		}
		tlsListener2 := tls.NewListener(listeners[1], s.httpServer.TLSConfig)
		go func() {
			if err := unixSrv.Serve(tlsListener2); err != http.ErrServerClosed {
				fmt.Printf("Unix HTTP server error: %v\n", err)
			}
		}()
	}

	return nil
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.auth.Handler(s.mux).ServeHTTP(w, r)
}

func (s *Server) setupRoutes() {
	// API discovery
	s.mux.HandleFunc("/api", s.handleAPIRoot)
	s.mux.HandleFunc("/api/v1", s.handleAPIV1)
	s.mux.HandleFunc("/apis", s.handleAPIsRoot)
	s.mux.HandleFunc("/apis/apps", s.handleAppsRoot)
	s.mux.HandleFunc("/apis/apps/v1", s.handleAppsV1)
	s.mux.HandleFunc("/apis/batch", s.handleBatchRoot)
	s.mux.HandleFunc("/apis/batch/v1", s.handleBatchV1)
	s.mux.HandleFunc("/version", s.handleVersion)
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/openapi/v2", s.handleOpenAPIv2)
	s.mux.HandleFunc("/openapi/v3", s.handleOpenAPIv3)
	s.mux.HandleFunc("/openapi/v3/", s.handleOpenAPIv3)

	// Cluster-scoped resource lists (used by kubectl get -A)
	for _, res := range []string{"pods", "services", "persistentvolumeclaims", "configmaps", "secrets"} {
		res := res
		s.mux.HandleFunc("/api/v1/"+res, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.handleClusterList(w, r, res)
		})
	}

	// Namespaced resources: /api/v1/namespaces/{ns}/...
	// Also handles /api/v1/namespaces/{ns} (GET/DELETE single namespace)
	s.mux.HandleFunc("/api/v1/namespaces/", s.handleNamespaced)

	// Cluster-scoped namespace list/create
	s.mux.HandleFunc("/api/v1/namespaces", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleNamespaceList(w, r)
		case http.MethodPost:
			s.handleNamespaceCreate(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// apps/v1 cluster-wide list
	s.mux.HandleFunc("/apis/apps/v1/deployments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleAppsClusterList(w, r)
	})

	// apps/v1 namespaced resources: /apis/apps/v1/namespaces/{ns}/deployments[/{name}]
	s.mux.HandleFunc("/apis/apps/v1/namespaces/", s.handleAppsNamespaced)

	// batch/v1 cluster-wide lists
	for _, res := range []string{"jobs", "cronjobs"} {
		res := res
		s.mux.HandleFunc("/apis/batch/v1/"+res, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			s.handleBatchClusterList(w, r, res)
		})
	}

	// batch/v1 namespaced resources: /apis/batch/v1/namespaces/{ns}/jobs|cronjobs[/{name}]
	s.mux.HandleFunc("/apis/batch/v1/namespaces/", s.handleBatchNamespaced)
}

// parseNamespaceResource parses a path like /api/v1/namespaces/{ns}/pods or /api/v1/namespaces/{ns}/pods/{name}
func parseNamespaceResource(path string) (namespace, resource, name string, ok bool) {
	parts := strings.TrimPrefix(path, "/api/v1/namespaces/")
	if parts == path {
		return "", "", "", false
	}
	parts = strings.TrimSuffix(parts, "/")
	if parts == "" {
		return "", "", "", false
	}

	pieces := strings.SplitN(parts, "/", 3)
	ns := pieces[0]
	if len(pieces) == 1 {
		return ns, "", "", true
	}
	resource = pieces[1]
	if len(pieces) == 2 {
		return ns, resource, "", true
	}
	return ns, resource, pieces[2], true
}

// parseBatchNamespaceResource parses /apis/batch/v1/namespaces/{ns}/{resource}[/{name}]
func parseBatchNamespaceResource(path string) (namespace, resource, name string, ok bool) {
	parts := strings.TrimPrefix(path, "/apis/batch/v1/namespaces/")
	if parts == path {
		return "", "", "", false
	}
	parts = strings.TrimSuffix(parts, "/")
	if parts == "" {
		return "", "", "", false
	}
	pieces := strings.SplitN(parts, "/", 3)
	ns := pieces[0]
	if len(pieces) == 1 {
		return ns, "", "", true
	}
	resource = pieces[1]
	if len(pieces) == 2 {
		return ns, resource, "", true
	}
	return ns, resource, pieces[2], true
}
