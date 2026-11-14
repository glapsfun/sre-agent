package report

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glapsfun/sre-agent/internal/safety"
)

type ExportReceipt struct {
	Path       string `json:"path"`
	DetailPath string `json:"detail_path,omitempty"`
	IndexPath  string `json:"index_path"`
}

const indexHeader = "<!-- sre-incident-index:v1 -->\n\n# Incident draft index\n\nAll entries are drafts requiring human review before sharing.\n\n"

// Export publishes a new draft and updates only a recognized managed index.
// Existing report paths are never replaced, including on repeated requests.
func Export(path string, doc Snapshot) (receipt ExportReceipt, err error) {
	base := filepath.Base(path)
	if strings.TrimSpace(path) == "" || strings.EqualFold(base, "index.md") || base == ".sre-export.lock" || safety.NewRedactor().Redact(base) != base || strings.ContainsAny(base, "\r\n") {
		return receipt, errors.New("invalid report output filename")
	}
	dir := filepath.Dir(path)
	receipt = ExportReceipt{Path: path, IndexPath: filepath.Join(dir, "index.md")}
	if doc.Detail != "" {
		receipt.DetailPath = filepath.Join(dir, strings.TrimSuffix(base, filepath.Ext(base))+".details.md")
	}
	lock := filepath.Join(dir, ".sre-export.lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ExportReceipt{}, errors.New("report export busy: active export or stale .sre-export.lock; inspect before retrying")
		}
		return ExportReceipt{}, err
	}
	defer os.Remove(lock)
	for _, target := range []string{receipt.Path, receipt.DetailPath} {
		if target == "" {
			continue
		}
		if _, err := os.Lstat(target); err == nil {
			return ExportReceipt{}, fmt.Errorf("report output already exists: %w", os.ErrExist)
		} else if !errors.Is(err, os.ErrNotExist) {
			return ExportReceipt{}, err
		}
	}
	index := indexHeader
	if info, err := os.Lstat(receipt.IndexPath); err == nil {
		if !info.Mode().IsRegular() || info.Size() > 4<<20 {
			return ExportReceipt{}, errors.New("report index must be a regular managed file within 4 MiB")
		}
		existing, err := os.ReadFile(receipt.IndexPath)
		if err != nil {
			return ExportReceipt{}, err
		}
		if !strings.HasPrefix(string(existing), indexHeader) {
			return ExportReceipt{}, errors.New("existing index.md is not a managed incident index")
		}
		index = string(existing)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ExportReceipt{}, err
	}
	var created []string
	defer func() {
		if err != nil {
			for _, name := range created {
				_ = os.Remove(name)
			}
		}
	}()
	summary := doc.Markdown
	if receipt.DetailPath != "" {
		summary += "\n[Detailed draft](" + relativeLink(filepath.Base(receipt.DetailPath)) + ")\n"
		if err = publishNew(receipt.DetailPath, doc.Detail); err != nil {
			return ExportReceipt{}, err
		}
		created = append(created, receipt.DetailPath)
	}
	if err = publishNew(path, summary); err != nil {
		return ExportReceipt{}, err
	}
	created = append(created, path)
	index += fmt.Sprintf("- [%s](%s): Draft; run %s; sequence %d; %s; generated %s.\n", literal(base), relativeLink(base), literal(doc.RunID), doc.ThroughSeq, literal(doc.Stage), doc.GeneratedAt.UTC().Format(time.RFC3339Nano))
	if err = WriteFile(receipt.IndexPath, Document{Markdown: index}); err != nil {
		return ExportReceipt{}, err
	}
	return receipt, nil
}

func relativeLink(name string) string {
	return "./" + strings.NewReplacer("(", "%28", ")", "%29").Replace(url.PathEscape(name))
}

func publishNew(path, text string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".sre-draft-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Hard-link publication is atomic and fails for every existing destination,
	// unlike rename, which replaces a file even after an earlier absence check.
	return os.Link(file.Name(), path)
}
