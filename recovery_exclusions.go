package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func safeRecoveryRelativePath(rel string) bool {
	return rel != "" && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, "../") && filepath.ToSlash(filepath.Clean(rel)) == rel
}

func preserveRecoveryDirectory(sourceRoot, targetRoot, rel string) error {
	current := sourceRoot
	for _, part := range strings.Split(rel, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("excluded backup storage is not a directory: %s", current)
		}
	}
	source := filepath.Join(sourceRoot, filepath.FromSlash(rel))
	return filepath.WalkDir(source, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		suffix, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		target := filepath.Join(targetRoot, suffix)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unsupported file in excluded backup storage")
		}
		return os.Link(path, target)
	})
}
