package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type runtimeContextReader struct {
	ctx context.Context
	in  io.Reader
}

func (r *runtimeContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.in.Read(p)
}

type runtimeBoundedWriter struct {
	ctx       context.Context
	out       io.Writer
	remaining int64
}

func (w *runtimeBoundedWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.remaining {
		return 0, fmt.Errorf("snapshot exceeds archive transfer limit")
	}
	n, err := w.out.Write(p)
	w.remaining -= int64(n)
	return n, err
}
func runtimeSnapshotHash(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, &runtimeContextReader{ctx, f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func inspectRuntimeSnapshot(ctx context.Context, path string, policy runtimeStateTransferPolicy) (runtimeStateSize, error) {
	return consumeRuntimeSnapshot(ctx, path, "", policy)
}

// The same archive reader validates downloads and local restores. Counting the
// whole decompressed stream also bounds hidden PAX headers and trailing gzip data.
func consumeRuntimeSnapshot(ctx context.Context, path, dest string, policy runtimeStateTransferPolicy) (runtimeStateSize, error) {
	var stats runtimeStateSize
	if err := validateRuntimeStatePolicy(policy); err != nil {
		return stats, err
	}
	f, err := os.Open(path)
	if err != nil {
		return stats, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return stats, err
	}
	if info.Size() > policy.MaxArchiveBytes {
		return stats, fmt.Errorf("snapshot exceeds archive transfer limit")
	}
	gz, err := gzip.NewReader(&runtimeContextReader{ctx, f})
	if err != nil {
		return stats, err
	}
	defer gz.Close()
	expanded := &io.LimitedReader{R: gz, N: policy.MaxExpandedBytes + policy.MaxEntries*8192 + (1 << 20) + 1}
	tr := tar.NewReader(expanded)
	seen := map[string]bool{}
	for {
		if err = ctx.Err(); err != nil {
			return stats, err
		}
		hdr, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return stats, e
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || strings.Contains(name, "\\") || seen[name] {
			return stats, fmt.Errorf("unsafe or duplicate snapshot entry")
		}
		stats.Entries++
		if hdr.Size < 0 || hdr.Size > policy.MaxExpandedBytes-stats.Bytes || int64(stats.Entries) > policy.MaxEntries {
			return stats, fmt.Errorf("snapshot expansion exceeds limits")
		}
		stats.Bytes += hdr.Size
		seen[name] = true
		switch hdr.Typeflag {
		case tar.TypeDir:
			if hdr.Size != 0 {
				return stats, fmt.Errorf("invalid snapshot directory size")
			}
			if dest != "" {
				if err = os.MkdirAll(filepath.Join(dest, name), 0700); err != nil {
					return stats, err
				}
			}
		case tar.TypeReg, tar.TypeRegA:
			var out io.Writer = io.Discard
			var file *os.File
			if dest != "" {
				target := filepath.Join(dest, name)
				if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					return stats, err
				}
				file, err = os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					return stats, err
				}
				out = file
			}
			_, err = io.CopyN(out, &runtimeContextReader{ctx, tr}, hdr.Size)
			if file != nil {
				if err == nil {
					err = file.Sync()
				}
				closeErr := file.Close()
				if err == nil {
					err = closeErr
				}
			}
			if err != nil {
				return stats, err
			}
		default:
			return stats, fmt.Errorf("snapshot links and special files are forbidden")
		}
	}
	// Read the trailer to verify gzip CRC and catch truncated/corrupt archives.
	if _, err = io.Copy(runtimeSnapshotPadding{}, &runtimeContextReader{ctx, expanded}); err != nil {
		return stats, err
	}
	if expanded.N == 0 {
		return stats, fmt.Errorf("snapshot expanded stream exceeds limits")
	}
	return stats, nil
}

func (m *appRuntimeManager) confirmRestoreTarget(ctx context.Context, d runtimeDeployment, op runtimeOperation, policy runtimeStateTransferPolicy) error {
	if d.Status != "prepared" && d.Status != "preparing" && d.Status != "stopped" {
		return fmt.Errorf("cannot restore state beneath a started or uncertain execution")
	}
	if d.Placement.local() && m.s.localApps.currentProc(d.InstallID) != nil {
		return fmt.Errorf("cannot restore state beneath a running local execution")
	}
	b, err := m.backend(d)
	if err != nil {
		return err
	}
	supported, err := intersectRuntimeStatePolicy(policy, runtimeBackendStatePolicy(b.Describe()))
	if err != nil || supported != policy {
		return fmt.Errorf("restore backend no longer supports persisted state transfer policy")
	}
	req, err := m.request(d, op, "inspect")
	if err != nil {
		return err
	}
	var observed runtimeExecution
	if remote, ok := b.(*remoteRuntimeBackend); ok {
		observed, err = remote.inspectContext(ctx, req)
	} else {
		observed, err = b.Inspect(req)
	}
	if err != nil {
		return fmt.Errorf("restore target shutdown could not be verified: %w", err)
	}
	if _, err = runtimeMergeExecution(d.Execution, observed); err != nil {
		return err
	}
	if observed.State != "prepared" && !(observed.State == "stopped" && observed.StoppedConfirmed) {
		return fmt.Errorf("restore target is not confirmed quiescent")
	}
	return nil
}
func (m *appRuntimeManager) checkSnapshotSource(ctx context.Context, d runtimeDeployment, op runtimeOperation, policy runtimeStateTransferPolicy) error {
	b, err := m.backend(d)
	if err != nil {
		return err
	}
	supported, err := intersectRuntimeStatePolicy(policy, runtimeBackendStatePolicy(b.Describe()))
	if err != nil || supported != policy {
		return fmt.Errorf("source backend no longer supports persisted state transfer policy")
	}
	req, err := m.request(d, op, "inspect")
	if err != nil {
		return err
	}
	var observed runtimeExecution
	if remote, ok := b.(*remoteRuntimeBackend); ok {
		observed, err = remote.inspectContext(ctx, req)
	} else {
		observed, err = b.Inspect(req)
	}
	if err != nil {
		return err
	}
	if _, err = runtimeMergeExecution(d.Execution, observed); err != nil {
		return err
	}
	if observed.State != "stopped" || !observed.StoppedConfirmed {
		return fmt.Errorf("snapshot requires freshly confirmed source shutdown")
	}
	var stats runtimeStateSize
	var free *int64
	if d.Placement.local() {
		stats, err = inspectRuntimeStateContext(ctx, m.localStateDir(d), policy)
		if err != nil {
			return err
		}
		n, e := runtimeStateDiskFree(m.localStateDir(d))
		if e != nil {
			return e
		}
		free = &n
	} else {
		if observed.StateBytes == nil || observed.StateEntries == nil || *observed.StateBytes < 0 || *observed.StateEntries < 0 || *observed.StateBytes > policy.MaxExpandedBytes || *observed.StateEntries > policy.MaxEntries {
			return fmt.Errorf("stopped source state exceeds policy or its size/entry count is unknown")
		}
		stats = runtimeStateSize{Bytes: *observed.StateBytes, Entries: int(*observed.StateEntries)}
		free = observed.StateAvailableBytes
	}
	archive, _ := runtimeStateDiskBudget(stats)
	if err = runtimeRequireDisk(free, archive, "Snapshot source"); err != nil {
		return err
	}
	available, err := runtimeStateDiskFree(m.s.localApps.cacheDir)
	if err != nil {
		return err
	}
	return runtimeRequireDisk(&available, archive, "Server snapshot cache")
}

// Tar permits zero padding after its end marker; silently ignoring another
// archive or non-zero payload would make the inspected contents ambiguous.
type runtimeSnapshotPadding struct{}

func (runtimeSnapshotPadding) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != 0 {
			return 0, fmt.Errorf("unexpected data after snapshot archive")
		}
	}
	return len(p), nil
}
