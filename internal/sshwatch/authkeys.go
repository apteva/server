package sshwatch

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// DefaultAuthorizedKeysPaths are the glob patterns watched by default.
var DefaultAuthorizedKeysPaths = []string{"/root/.ssh/authorized_keys", "/home/*/.ssh/authorized_keys", "/etc/ssh/authorized_keys/*"}

const maxAuthorizedKeysBytes = 1 << 20

// ParseAuthorizedKeys returns the canonical key set of an authorized_keys
// file: fingerprints, types, comments and an options digest. Key material and
// option values are never retained. Unparseable lines are ignored.
func ParseAuthorizedKeys(data []byte) []AuthorizedKey {
	byID := map[string]AuthorizedKey{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), maxAuthorizedKeysBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		pk, comment, options, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			continue
		}
		k := AuthorizedKey{Fingerprint: ssh.FingerprintSHA256(pk), Type: pk.Type(), Comment: cleanField(comment)}
		if len(options) > 0 {
			sum := sha256.Sum256([]byte(strings.Join(options, "\x00")))
			k.OptionsHash = hex.EncodeToString(sum[:8])
		}
		id := k.Fingerprint + "|" + k.OptionsHash
		if _, dup := byID[id]; !dup {
			byID[id] = k
		}
	}
	out := make([]AuthorizedKey, 0, len(byID))
	for _, k := range byID {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Fingerprint != out[j].Fingerprint {
			return out[i].Fingerprint < out[j].Fingerprint
		}
		return out[i].OptionsHash < out[j].OptionsHash
	})
	return out
}

// keysDigest covers what grants access (key + options), not comments.
func keysDigest(keys []AuthorizedKey) string {
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s|%s|%s\n", k.Fingerprint, k.Type, k.OptionsHash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// DiffAuthorizedKeys returns entries present only in next (added) and only in
// prev (removed). An options change shows as remove + add of one fingerprint.
func DiffAuthorizedKeys(prev, next []AuthorizedKey) (added, removed []AuthorizedKey) {
	id := func(k AuthorizedKey) string { return k.Fingerprint + "|" + k.OptionsHash }
	old := map[string]bool{}
	for _, k := range prev {
		old[id(k)] = true
	}
	cur := map[string]bool{}
	for _, k := range next {
		cur[id(k)] = true
		if !old[id(k)] {
			added = append(added, k)
		}
	}
	for _, k := range prev {
		if !cur[id(k)] {
			removed = append(removed, k)
		}
	}
	return added, removed
}

func expandKeyPaths(patterns []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			continue
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out
}

// userForKeyPath derives the account an authorized_keys path belongs to.
func userForKeyPath(path string) string {
	clean := filepath.Clean(path)
	switch {
	case strings.HasPrefix(clean, "/root/"):
		return "root"
	case strings.HasPrefix(clean, "/home/"):
		user, _, _ := strings.Cut(strings.TrimPrefix(clean, "/home/"), "/")
		return cleanField(user)
	case strings.HasPrefix(clean, "/etc/ssh/authorized_keys/"):
		return cleanField(filepath.Base(clean))
	}
	return ""
}

func readKeysFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return io.ReadAll(io.LimitReader(f, maxAuthorizedKeysBytes))
}

// scanAuthorizedKeys compares every watched file with its stored snapshot and
// records one event per change. Snapshot and event commit together.
func (c *Collector) scanAuthorizedKeys(ctx context.Context) error {
	st := c.currentStore()
	if st == nil {
		return errors.New("store is not open")
	}
	known, err := st.authorizedKeysPaths(ctx)
	if err != nil {
		return err
	}
	paths := expandKeyPaths(c.cfg.AuthorizedKeysPaths)
	seen := map[string]bool{}
	for _, p := range paths {
		seen[p] = true
	}
	for _, p := range known {
		if !seen[p] {
			paths = append(paths, p)
		}
	}
	type observed struct {
		path string
		keys []AuthorizedKey
	}
	var files []observed
	var problems []string
	for _, p := range paths {
		data, err := readKeysFile(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			files = append(files, observed{path: p})
		case err != nil:
			// Unreadable is unknown, not "all keys removed".
			problems = append(problems, err.Error())
		default:
			files = append(files, observed{path: p, keys: ParseAuthorizedKeys(data)})
		}
	}
	policy := c.currentPolicy()
	now := c.now().UTC()
	return st.Update(ctx, func(tx *Tx) error {
		baselineDone, err := tx.Meta(metaKeysBaseline)
		if err != nil {
			return err
		}
		for _, f := range files {
			digest := keysDigest(f.keys)
			snap, found, err := tx.authorizedKeys(f.path)
			if err != nil {
				return err
			}
			if found && snap.Digest == digest {
				continue
			}
			if !found && len(f.keys) == 0 {
				continue
			}
			added, removed := DiffAuthorizedKeys(snap.Keys, f.keys)
			gen := snap.Generation + 1
			ev := Event{
				ID:          eventID(c.machineID, "authorized_keys:"+f.path+":"+strconv.FormatInt(gen, 10)+":"+digest),
				MachineID:   c.machineID,
				Hostname:    c.hostname,
				ObservedAt:  now,
				Kind:        KindAuthorizedKeysChange,
				Result:      ResultChanged,
				User:        userForKeyPath(f.path),
				Path:        f.path,
				KeysAdded:   added,
				KeysRemoved: removed,
				KeyCount:    len(f.keys),
				Source:      "authorized_keys",
			}
			if !found && baselineDone != "1" {
				ev.Result = ResultBaseline
				ev.Message = fmt.Sprintf("initial inventory: %d key(s)", len(f.keys))
			} else {
				ev.Message = fmt.Sprintf("%d key(s) added, %d removed; %d present", len(added), len(removed), len(f.keys))
			}
			if _, err := tx.InsertEvent(&ev, policy); err != nil {
				return err
			}
			if err := tx.setAuthorizedKeys(f.path, keysSnapshot{Digest: digest, Generation: gen, Keys: f.keys}); err != nil {
				return err
			}
		}
		if err := tx.SetMeta(metaKeysBaseline, "1"); err != nil {
			return err
		}
		return tx.SetMeta(metaKeysError, truncate(strings.Join(problems, "; "), 1024))
	})
}

func (c *Collector) keysLoop(ctx context.Context) {
	if len(c.cfg.AuthorizedKeysPaths) == 0 {
		return
	}
	for {
		if err := c.scanAuthorizedKeys(ctx); err != nil && ctx.Err() == nil {
			c.logf("authorized_keys scan: %v", err)
		}
		if !sleepCtx(ctx, c.cfg.AuthorizedKeysInterval) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
