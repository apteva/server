package framework

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
)

// AppCtx is the bundle of platform services handed to an app. Single
// instance per app slug; safe to share across goroutines.
//
// DB is the same shared SQLite the rest of the server uses. Apps
// should name tables with the slug prefix (e.g. "channel_chat_chats")
// so cross-app conflicts are impossible and ops tooling can tell at a
// glance which rows belong to whom. The convention isn't enforced at
// the driver level in v1; it's a lint rule + a review norm.
type AppCtx struct {
	Slug    string
	Manifest Manifest
	DB      *sql.DB
	Bus     *AppBus
	Logger  *slog.Logger
	// Ctx is cancelled on server shutdown. Workers use this to
	// exit cleanly; request handlers that launch background work
	// should derive from it.
	Ctx context.Context

	// CallApp is the placeholder for inter-app RPC. Stage 1 only
	// has one app, so it's a no-op that returns ErrNoApp. Wired
	// properly once we have two apps.
	CallApp func(slug, fn string, args any) (any, error)
}

// TablePrefix is the canonical prefix apps should use for their tables.
// "channel-chat" → "channel_chat_". Dashes become underscores because
// SQLite (and just about every other SQL dialect) dislikes them in
// identifiers.
func TablePrefix(slug string) string {
	return strings.ReplaceAll(slug, "-", "_") + "_"
}

// SharedSecret is exposed so apps that need to encrypt per-row data
// (future: app-scoped credential fields) can use the same key the
// rest of the server does. Not populated in v1 stubs — set by the
// framework loader when an app actually declares a need.
type SharedSecret = []byte
