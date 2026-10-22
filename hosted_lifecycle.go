package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/apteva/server/internal/host"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The standby gate is deliberately before boot counters, migrations, restore
// markers, secrets, sidecar adoption and all background workers. Only one slot
// can cross it for a given installation; a candidate never opens a second DB.
type hostedLifecycle struct {
	key      []byte
	mu       sync.RWMutex
	app      *Server
	preserve atomic.Bool
	server   *http.Server
	listener net.Listener
	release  func()
}

func enterHostedLifecycle() (*hostedLifecycle, error) {
	socket := os.Getenv("APTEVA_HOST_SLOT_SOCKET")
	if socket != "" {
		signal.Ignore(syscall.SIGPIPE)
	}
	if socket == "" {
		return nil, nil
	}
	home, err := filepath.Abs(os.Getenv("APTEVA_HOME"))
	if err != nil {
		return nil, err
	}
	if home == "/" || filepath.Dir(socket) != filepath.Join(home, "host") || os.Getenv("APTEVA_HOST_BACKEND_ADDR") == "" {
		return nil, errors.New("invalid hosted installation identity")
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(socket, 0600); err != nil {
		l.Close()
		return nil, err
	}
	h := &hostedLifecycle{listener: l}
	activate := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		identity := sha256.Sum256([]byte(os.Getenv("APTEVA_HOST_PROXY_SECRET")))
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": host.Protocol, "version": CLIVersion, "pid": os.Getpid(), "proxy_identity": hex.EncodeToString(identity[:])})
	})
	mux.HandleFunc("POST /prepare", func(w http.ResponseWriter, r *http.Request) {
		var expected host.DatabaseIdentity
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&expected) != nil {
			http.Error(w, "invalid preparation", 400)
			return
		}
		data := os.Getenv("DATA_DIR")
		if data == "" {
			data = os.Getenv("APTEVA_HOME")
		}
		key, err := LoadExistingSecret(data)
		if err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		sum := sha256.Sum256(key)
		if expected.SecretHash == "" || expected.SchemaHash == "" || expected.SecretHash != hex.EncodeToString(sum[:]) {
			http.Error(w, "existing encryption key changed", 409)
			return
		}
		if err = validateHostedSchema(os.Getenv("DB_PATH"), expected.SchemaHash); err != nil {
			http.Error(w, err.Error(), 409)
			return
		}
		h.mu.Lock()
		h.key = append([]byte(nil), key...)
		h.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /identity", func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		s := h.app
		h.mu.RUnlock()
		if s == nil {
			http.Error(w, "starting", 503)
			return
		}
		schema, err := updateDatabaseSchema(s.dbPath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		sum := sha256.Sum256(s.secret)
		_ = json.NewEncoder(w).Encode(host.DatabaseIdentity{Version: CLIVersion, SchemaHash: schema, SecretHash: hex.EncodeToString(sum[:])})
	})
	mux.HandleFunc("POST /certificate", func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		s := h.app
		h.mu.RUnlock()
		if s == nil || s.ingressCerts == nil {
			http.Error(w, "certificate manager is starting", 503)
			return
		}
		var req host.CertificateRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, "invalid certificate request", 400)
			return
		}
		cert, err := s.ingressCerts.GetCertificate(&tls.ClientHelloInfo{ServerName: req.ServerName, SupportedProtos: req.Protocols})
		if err != nil {
			http.Error(w, err.Error(), 503)
			return
		}
		key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
		if err != nil {
			http.Error(w, "cannot encode certificate", 500)
			return
		}
		_ = json.NewEncoder(w).Encode(host.CertificateResponse{Chain: cert.Certificate, Key: key})
	})
	mux.HandleFunc("GET /policy", func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		s := h.app
		h.mu.RUnlock()
		if s == nil {
			http.Error(w, "starting", 503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"agent_policy": s.agentUpdatePolicy()})
	})
	mux.HandleFunc("POST /rollout", func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		s := h.app
		h.mu.RUnlock()
		if s == nil || !s.ready.Load() {
			http.Error(w, "starting", 503)
			return
		}
		var body struct {
			Policy string `json:"agent_policy"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "invalid policy", 400)
			return
		}
		if body.Policy == "preserve" {
			w.WriteHeader(204)
			return
		}
		if body.Policy != "restart" && body.Policy != "rolling" {
			http.Error(w, "invalid policy", 400)
			return
		}
		delay := time.Duration(0)
		if body.Policy == "rolling" {
			delay = s.agentRolloutDelay()
		}
		_, err := s.startCoreRollout(nil, "", "all", true, delay)
		if err != nil && err.Error() != "no running agents require a core update" {
			http.Error(w, err.Error(), 409)
			return
		}
		w.WriteHeader(204)
	})
	runtimeHandler := func(w http.ResponseWriter, r *http.Request) {
		h.mu.RLock()
		s := h.app
		h.mu.RUnlock()
		if s == nil || !s.ready.Load() {
			http.Error(w, "server is not ready", 503)
			return
		}
		if r.URL.Path == "/unfreeze" {
			s.unfreezeHostedWork()
			w.WriteHeader(204)
			return
		}
		frozen := r.URL.Path == "/freeze"
		if frozen {
			if err := s.freezeHostedWork(); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
		}
		pids, err := s.hostedRuntimePIDs()
		if err != nil {
			if frozen {
				s.unfreezeHostedWork()
			}
			http.Error(w, err.Error(), 409)
			return
		}
		_ = json.NewEncoder(w).Encode(pids)
	}
	mux.HandleFunc("GET /runtimes", runtimeHandler)
	mux.HandleFunc("POST /freeze", runtimeHandler)
	mux.HandleFunc("POST /unfreeze", runtimeHandler)
	mux.HandleFunc("POST /activate", func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(activate) }); w.WriteHeader(204) })
	mux.HandleFunc("POST /retire", func(w http.ResponseWriter, r *http.Request) { h.preserve.Store(true); w.WriteHeader(204) })
	h.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go h.server.Serve(l)
	<-activate
	h.release, err = host.Lock(filepath.Join(home, "host", "runtime.lock"), false)
	if err != nil {
		h.Close()
		return nil, fmt.Errorf("refusing duplicate runtime ownership: %w", err)
	}
	return h, nil
}
func (h *hostedLifecycle) Close() {
	if h == nil {
		return
	}
	if h.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = h.server.Shutdown(ctx)
	}
	if h.release != nil {
		h.release()
	}
}
func hostedPreserve(h *hostedLifecycle) bool { return h != nil && h.preserve.Load() }

func markPlatformAgentsForShutdown(store *Store, preserve bool) (int64, error) {
	if preserve {
		return 0, nil
	}
	return store.MarkPlatformAgentsStoppedForShutdown()
}

func (h *hostedLifecycle) Attach(s *Server) {
	if h != nil {
		h.mu.Lock()
		h.app = s
		h.mu.Unlock()
	}
}

// Authenticate the private ingress hop before restoring the original peer.
// Otherwise every internet request would appear to be an internal callback.
func hostedIngress(next http.Handler) http.Handler {
	secret := os.Getenv("APTEVA_HOST_PROXY_SECRET")
	if secret == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Apteva-Host-Token")), []byte(secret)) != 1 {
			http.Error(w, "private server listener", http.StatusForbidden)
			return
		}
		if remote := r.Header.Get("X-Apteva-Host-Remote-Addr"); remote != "" {
			r.RemoteAddr = remote
		}
		if r.Header.Get("X-Apteva-Host-Scheme") == "https" {
			r.TLS = &tls.ConnectionState{HandshakeComplete: true}
			r.URL.Scheme = "https"
		}
		r.Header.Del("X-Apteva-Host-Token")
		r.Header.Del("X-Apteva-Host-Remote-Addr")
		r.Header.Del("X-Apteva-Host-Scheme")
		next.ServeHTTP(w, r)
	})
}

func requestHostedListeners(httpAddr, httpsAddr string) error {
	return requestHostedListenerOwner("instance", httpAddr, httpsAddr)
}
func requestHostedListenerOwner(owner, httpAddr, httpsAddr string) error {
	socket := os.Getenv("APTEVA_HOST_SOCKET")
	if socket == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := host.UnixClient(socket)
	defer client.CloseIdleConnections()
	return host.Call(ctx, client, "POST", "/listeners", host.ListenerRequest{Owner: owner, HTTP: httpAddr, HTTPS: httpsAddr}, nil)
}

func (h *hostedLifecycle) Secret() []byte {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]byte(nil), h.key...)
}

func withoutHostedEnvironment(base []string) []string {
	out := make([]string, 0, len(base))
	for _, v := range base {
		if !strings.HasPrefix(v, "APTEVA_HOST_") {
			out = append(out, v)
		}
	}
	return out
}
