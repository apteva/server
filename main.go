package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
)

type Server struct {
	store        *Store
	instances    *InstanceManager
	fireworksKey string
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "backplane.db"
	}

	cogitoCmd := os.Getenv("COGITO_CMD")
	if cogitoCmd == "" {
		cogitoCmd = "cogito"
	}

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}

	fireworksKey := os.Getenv("FIREWORKS_API_KEY")

	store, err := NewStore(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	s := &Server{
		store:        store,
		instances:    NewInstanceManager(dataDir, cogitoCmd, 3210),
		fireworksKey: fireworksKey,
	}

	mux := http.NewServeMux()

	// Public routes (no auth)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/auth/register", s.handleRegister)
	mux.HandleFunc("/auth/login", s.handleLogin)

	// Authenticated routes
	mux.HandleFunc("/auth/keys", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListKeys(w, r)
		case http.MethodPost:
			s.handleCreateKey(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))
	mux.HandleFunc("/auth/keys/", s.authMiddleware(s.handleDeleteKey))

	mux.HandleFunc("/instances", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.handleListInstances(w, r)
		case http.MethodPost:
			s.handleCreateInstance(w, r)
		default:
			http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
		}
	}))

	// Instance routes — need to distinguish /instances/:id from /instances/:id/...
	mux.HandleFunc("/instances/", s.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/instances/")

		// /instances/:id/config
		if strings.HasSuffix(path, "/config") {
			s.handleUpdateConfig(w, r)
			return
		}

		// /instances/:id/status, /instances/:id/threads, etc. → proxy
		if strings.Contains(path, "/") {
			s.handleProxy(w, r)
			return
		}

		// /instances/:id
		s.handleInstance(w, r)
	}))

	fmt.Fprintf(os.Stderr, "backplane running on :%s\n", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

func itoa64(n int64) string {
	return strconv.FormatInt(n, 10)
}

func atoi64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
