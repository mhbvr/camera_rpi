package camera

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// LoadConfig reads path, falling back to Defaults when it does not exist.
// Fields missing from the file keep their default values.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Defaults(), nil
	}
	if err != nil {
		return Defaults(), err
	}
	c, err := Merge(Defaults(), b)
	if err != nil {
		return Defaults(), err
	}
	if err := Validate(c, nil); err != nil {
		return Defaults(), err
	}
	return c, nil
}

// SaveConfig writes c to path atomically (temp file + rename).
func SaveConfig(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
