package managedmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func Write(sourceDir string, input Definition) error {
	def := Normalize(input)
	if err := Validate(def); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(sourceDir, "tools"), 0o700); err != nil {
		return err
	}
	for i := range def.Tools {
		path, err := HandlerPath(sourceDir, def.Tools[i].Handler)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := writeAtomic(path, []byte(def.Tools[i].Code), 0o600); err != nil {
			return fmt.Errorf("write handler %q: %w", def.Tools[i].Name, err)
		}
		def.Tools[i].Code = ""
	}
	raw, err := json.MarshalIndent(def, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeAtomic(filepath.Join(sourceDir, "server.json"), raw, 0o600)
}

func writeAtomic(path string, raw []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
