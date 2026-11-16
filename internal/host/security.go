package host

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/apteva/server/internal/sshwatch"
)

// Collector modes for Config.SecurityCollector / APTEVA_SECURITY_COLLECTOR.
const (
	SecurityCollectorOn     = "on"
	SecurityCollectorReader = "reader"
	SecurityCollectorOff    = "off"
)

// startSecurity starts the in-process SSH collector. It collects only when it
// wins the machine-level lock (a standalone `apteva-host security-collector`
// unit may hold it) and otherwise serves the shared store read-only. Collector
// problems never prevent the host from serving the installation.
func (h *Host) startSecurity(ctx context.Context) (done <-chan struct{}) {
	mode := strings.ToLower(strings.TrimSpace(h.cfg.SecurityCollector))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(os.Getenv("APTEVA_SECURITY_COLLECTOR")))
	}
	if mode == SecurityCollectorOff || mode == "0" || mode == "false" {
		return nil
	}
	var cfg sshwatch.Config
	if h.cfg.Security != nil {
		cfg = *h.cfg.Security
	} else {
		var err error
		if cfg, err = sshwatch.ConfigFromEnv(h.cfg.Home); err != nil {
			log.Printf("[HOST] SSH collector disabled: %v", err)
			return nil
		}
	}
	if cfg.Dir == "" {
		dir, err := sshwatch.DefaultDir(h.cfg.Home)
		if err != nil {
			log.Printf("[HOST] SSH collector disabled: %v", err)
			return nil
		}
		cfg.Dir = dir
	}
	if mode == SecurityCollectorReader {
		cfg.ReadOnly = true
	}
	collector, err := sshwatch.New(cfg)
	if err != nil {
		log.Printf("[HOST] SSH collector disabled: %v", err)
		return nil
	}
	h.security = collector
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer collector.Close()
		_ = collector.Run(ctx)
	}()
	return finished
}

func (h *Host) securityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /security/events", func(w http.ResponseWriter, r *http.Request) {
		c := h.securityCollector(w)
		if c == nil {
			return
		}
		q := r.URL.Query()
		after, limit := int64(0), 500
		var err error
		if v := q.Get("after"); v != "" {
			if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
				http.Error(w, "invalid after", http.StatusBadRequest)
				return
			}
		}
		if v := q.Get("limit"); v != "" {
			if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
				http.Error(w, "invalid limit", http.StatusBadRequest)
				return
			}
		}
		if limit > 2000 {
			limit = 2000
		}
		page, err := c.Events(r.Context(), after, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, page)
	})
	mux.HandleFunc("GET /security/health", func(w http.ResponseWriter, r *http.Request) {
		if c := h.securityCollector(w); c != nil {
			writeJSON(w, c.Health(r.Context()))
		}
	})
	mux.HandleFunc("GET /security/policy", func(w http.ResponseWriter, r *http.Request) {
		c := h.securityCollector(w)
		if c == nil {
			return
		}
		p, err := c.Policy()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, p)
	})
	mux.HandleFunc("PUT /security/policy", func(w http.ResponseWriter, r *http.Request) {
		c := h.securityCollector(w)
		if c == nil {
			return
		}
		var p sshwatch.Policy
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&p); err != nil {
			http.Error(w, "invalid policy", http.StatusBadRequest)
			return
		}
		applied, err := c.SetPolicy(p)
		if err != nil {
			var verr *sshwatch.ValidationError
			if errors.As(err, &verr) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, applied)
	})
}

func (h *Host) securityCollector(w http.ResponseWriter) *sshwatch.Collector {
	if h.security == nil {
		http.Error(w, "SSH security collector is disabled on this host", http.StatusServiceUnavailable)
		return nil
	}
	return h.security
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
