package main

// Public GitHub archive fallback for source installs on hosts without Git.
// Git remains the preferred path (including private/non-GitHub repositories).

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const githubArchiveAPI = "https://api.github.com/repos"

var githubRepoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func publicGitHubRepo(repoURL string) (string, string, error) {
	parsed, err := url.Parse(repoURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("git is unavailable; archive fallback supports public https://github.com repositories only")
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(parsed.Path, ".git"), "/"), "/")
	if len(parts) != 2 || !githubRepoPart.MatchString(parts[0]) || !githubRepoPart.MatchString(parts[1]) {
		return "", "", fmt.Errorf("invalid public GitHub repository %q", repoURL)
	}
	return parts[0], parts[1], nil
}

func clonePublicGitHubArchive(srcDir, repoURL, ref string, sparsePaths []string) error {
	client := &http.Client{Timeout: 15 * time.Minute}
	return clonePublicGitHubArchiveFrom(client, githubArchiveAPI, srcDir, repoURL, ref, sparsePaths)
}

func clonePublicGitHubArchiveFrom(client *http.Client, baseURL, srcDir, repoURL, ref string, sparsePaths []string) error {
	owner, repo, err := publicGitHubRepo(repoURL)
	if err != nil {
		return err
	}
	if ref == "" {
		ref = "main"
	}
	if err := os.MkdirAll(filepath.Dir(srcDir), 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(srcDir), ".clone-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	archiveURL := strings.TrimRight(baseURL, "/") + "/" + owner + "/" + repo + "/tarball/" + url.PathEscape(ref)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "apteva-source-installer")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download GitHub source archive: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub source archive returned HTTP %d", response.StatusCode)
	}
	compressed := &io.LimitedReader{R: response.Body, N: 256 << 20}
	zipper, err := gzip.NewReader(compressed)
	if err != nil {
		return fmt.Errorf("read GitHub source archive: %w", err)
	}
	defer zipper.Close()
	reader := tar.NewReader(zipper)
	var archiveRoot string
	var files int
	var extracted int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read GitHub source archive: %w", err)
		}
		name := header.Name
		if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
			return fmt.Errorf("unsafe GitHub archive path %q", name)
		}
		parts := strings.SplitN(name, "/", 2)
		if len(parts) < 2 {
			// Some tar producers emit a rootless global metadata header.
			// It must not become the root prefix for actual source files.
			continue
		}
		if archiveRoot == "" {
			archiveRoot = parts[0]
		}
		if parts[0] != archiveRoot {
			continue
		}
		relative := path.Clean(parts[1])
		if relative == "." {
			continue
		}
		if relative == ".." || strings.HasPrefix(relative, "../") || strings.HasPrefix(relative, "/") {
			return fmt.Errorf("unsafe GitHub archive path %q", name)
		}
		if len(sparsePaths) > 0 {
			included := false
			for _, sparse := range sparsePaths {
				if relative == sparse || strings.HasPrefix(relative, sparse+"/") {
					included = true
					break
				}
			}
			if !included {
				continue
			}
		}
		target := filepath.Join(stage, filepath.FromSlash(relative))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			extracted += header.Size
			if extracted > 1<<30 {
				return errors.New("GitHub source archive exceeds extracted size limit")
			}
			if err := writeGoArchiveFile(target, reader, header.Size, header.FileInfo().Mode()); err != nil {
				return err
			}
			files++
		default:
			return fmt.Errorf("unsupported GitHub archive entry %q (type %d)", name, header.Typeflag)
		}
	}
	if compressed.N == 0 {
		return errors.New("GitHub source archive exceeds download size limit")
	}
	if files == 0 {
		return errors.New("GitHub source archive did not contain the requested app path")
	}
	if err := os.RemoveAll(srcDir); err != nil {
		return err
	}
	if err := os.Rename(stage, srcDir); err != nil {
		return err
	}
	return nil
}
