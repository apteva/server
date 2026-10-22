package main

import (
	"context"
	"fmt"
	"github.com/apteva/server/internal/host"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
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
	bind := os.Getenv("APTEVA_BIND")
	if bind == "" {
		bind = "127.0.0.1"
		if os.Geteuid() == 0 {
			bind = "0.0.0.0"
		}
	}
	binary := os.Getenv("APTEVA_SERVER_BIN")
	if binary == "" {
		self, _ := os.Executable()
		binary = filepath.Join(filepath.Dir(self), "apteva-server")
	}
	h, err := host.New(host.Config{Home: home, DBPath: db, ListenAddr: bind + ":" + port, ServerBinary: binary})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err = h.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
