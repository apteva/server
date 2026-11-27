package main

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// serverDBPath resolves the database the same way normal startup does:
// DB_PATH, else APTEVA_HOME/apteva.db (the CLI and service layout), else the
// legacy apteva-server.db in the working directory (Docker and standalone).
func serverDBPath() string {
	if p := os.Getenv("DB_PATH"); p != "" {
		return p
	}
	if home := os.Getenv("APTEVA_HOME"); home != "" {
		return filepath.Join(home, "apteva.db")
	}
	return "apteva-server.db"
}

// runResetPassword is the internal --reset-password mode behind
// `apteva reset-password`: access to the host's files is the proof of
// ownership, so a locked-out admin can get back in without email or another
// admin. The new password arrives on stdin. It works whether or not the
// server is running, since sessions are checked against the database on every
// request. It never creates or migrates a database.
func runResetPassword(dbPath, email string, in io.Reader, out io.Writer) error {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read new password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return errors.New("no new password on stdin")
	}
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no Apteva database at %s", dbPath)
		}
		return err
	}
	store, err := OpenExistingStore(dbPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer store.Close()

	var userID int64
	var accountEmail string
	err = store.dbQueryRow(`SELECT id, email FROM users WHERE lower(email) = lower(?)`, strings.TrimSpace(email)).Scan(&userID, &accountEmail)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no account with email %q%s", email, adminAccountsHint(store))
	}
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := store.UpdateUserPassword(userID, string(hash)); err != nil {
		return err
	}
	if err := store.DeleteSessionsForUser(userID); err != nil {
		return fmt.Errorf("password changed, but signing out existing sessions failed: %w", err)
	}
	if err := store.DeletePasswordResetTokensForUser(userID); err != nil {
		return fmt.Errorf("password changed, but removing pending reset links failed: %w", err)
	}
	fmt.Fprintf(out, "Password reset for %s. Every existing session for this account was signed out.\n", accountEmail)
	return nil
}

// adminAccountsHint names the admin accounts, to help an operator who does
// not remember which email they signed up with.
func adminAccountsHint(store *Store) string {
	rows, err := store.dbQuery(`SELECT email FROM users WHERE role = 'admin' ORDER BY id`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var emails []string
	for rows.Next() {
		var e string
		if rows.Scan(&e) == nil {
			emails = append(emails, e)
		}
	}
	if len(emails) == 0 {
		return ""
	}
	return " (admin accounts: " + strings.Join(emails, ", ") + ")"
}
