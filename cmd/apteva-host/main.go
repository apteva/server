package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/apteva/server/internal/host"
	"github.com/apteva/server/internal/sshwatch"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if len(os.Args) > 1 && os.Args[1] == "security-collector" {
		if err := runSecurityCollector(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	home := os.Getenv("APTEVA_HOME")
	if home == "" {
		fmt.Fprintln(os.Stderr, "APTEVA_HOME is required")
		os.Exit(2)
	}
	db := os.Getenv("DB_PATH")
	if db == "" {
		db = filepath.Join(home, "apteva.db")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "5280"
	}
	bind, warning := managementBind(os.Getenv("APTEVA_BIND"))
	if warning != "" {
		log.Printf("[HOST] WARNING: %s", warning)
	}
	binary := os.Getenv("APTEVA_SERVER_BIN")
	if binary == "" {
		self, _ := os.Executable()
		binary = filepath.Join(filepath.Dir(self), "apteva-server")
	}
	h, err := host.New(host.Config{Home: home, DBPath: db, ListenAddr: net.JoinHostPort(bind, port), ServerBinary: binary})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = h.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// managementBind returns the management listener address. It is loopback
// unless APTEVA_BIND explicitly says otherwise (running as root no longer
// implies a public bind); public website listeners are requested separately
// through the control socket.
func managementBind(explicit string) (bind, warning string) {
	if explicit == "" {
		return "127.0.0.1", ""
	}
	if ip := net.ParseIP(explicit); ip != nil && ip.IsLoopback() || explicit == "localhost" {
		return explicit, ""
	}
	return explicit, fmt.Sprintf("management listener bound to non-loopback address %q via APTEVA_BIND; the admin API is reachable from the network", explicit)
}

// runSecurityCollector runs only the machine-level SSH collector, without an
// installation home, so it can run as its own narrowly privileged unit.
func runSecurityCollector(ctx context.Context) error {
	cfg, err := sshwatch.ConfigFromEnv(os.Getenv("APTEVA_HOME"))
	if err != nil {
		return err
	}
	if v := os.Getenv("APTEVA_SECURITY_COLLECTOR"); v == host.SecurityCollectorReader {
		cfg.ReadOnly = true
	}
	c, err := sshwatch.New(cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	log.Printf("[SECURITY] SSH collector using %s", c.Dir())
	return c.Run(ctx)
}
