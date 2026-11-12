package report

import (
	"os"
	"path/filepath"
)

// WriteFile atomically replaces path with finalized, sanitized Markdown.
func WriteFile(path string, document Document) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".sre-report-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.WriteString(document.Markdown); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
