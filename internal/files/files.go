// Package files classifies and stores chat attachments (DESIGN §17.4): the mime type
// is sniffed server-side against an extension allowlist (the client's claim is
// ignored), bytes are kept by sha256, and PDFs can be turned into text.
package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

// Classes.
const (
	ClassText  = "text"
	ClassPDF   = "pdf"
	ClassImage = "image"
)

// Limits per class (the per-upload max_file_mb applies on top).
const (
	MaxTextBytes  = 2 << 20
	MaxImageBytes = 5 << 20
)

// textExt maps allowed text extensions to the served mime type.
var textExt = map[string]string{
	".txt": "text/plain", ".md": "text/markdown", ".markdown": "text/markdown", ".csv": "text/csv",
	".tsv": "text/tab-separated-values", ".json": "application/json", ".jsonl": "application/json",
	".log": "text/plain", ".yaml": "text/plain", ".yml": "text/plain", ".toml": "text/plain", ".ini": "text/plain",
	".cfg": "text/plain", ".conf": "text/plain", ".xml": "text/plain", ".rst": "text/plain", ".org": "text/plain",
	".tex": "text/plain", ".diff": "text/plain", ".patch": "text/plain", ".sql": "text/plain",
	".go": "text/plain", ".py": "text/plain", ".js": "text/plain", ".mjs": "text/plain", ".ts": "text/plain",
	".tsx": "text/plain", ".jsx": "text/plain", ".c": "text/plain", ".h": "text/plain", ".cc": "text/plain",
	".cpp": "text/plain", ".hpp": "text/plain", ".java": "text/plain", ".kt": "text/plain", ".rs": "text/plain",
	".rb": "text/plain", ".php": "text/plain", ".sh": "text/plain", ".bash": "text/plain", ".zsh": "text/plain",
	".lua": "text/plain", ".pl": "text/plain", ".r": "text/plain", ".swift": "text/plain", ".cs": "text/plain",
	".scala": "text/plain", ".hs": "text/plain", ".ml": "text/plain", ".ex": "text/plain", ".exs": "text/plain",
	".erl": "text/plain", ".clj": "text/plain", ".css": "text/plain", ".scss": "text/plain", ".vue": "text/plain",
	".proto": "text/plain", ".graphql": "text/plain", ".tf": "text/plain", ".nix": "text/plain", ".zig": "text/plain",
}

// textNames are allowed text files without an extension.
var textNames = map[string]bool{"Dockerfile": true, "Makefile": true, "README": true, "LICENSE": true}

var imageExt = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp"}

// Error is a refusal meant for the user.
type Error string

func (e Error) Error() string { return string(e) }

// Classify decides what a file is from its name and content. It returns the mime type
// the hub serves and stores, and the class used to expand it into a request.
func Classify(name string, data []byte) (mime, class string, err error) {
	base := filepath.Base(name)
	ext := strings.ToLower(filepath.Ext(base))
	sniff := http.DetectContentType(data)
	switch ext {
	case ".svg", ".svgz", ".html", ".htm", ".xhtml", ".shtml", ".mht", ".mhtml":
		return "", "", Error("SVG and HTML files are not accepted")
	}
	if m, ok := imageExt[ext]; ok {
		if sniff != m && !(m == "image/jpeg" && sniff == "image/jpeg") {
			return "", "", Error(fmt.Sprintf("%s does not look like a %s file", base, ext))
		}
		if len(data) > MaxImageBytes {
			return "", "", Error("images are limited to 5 MB")
		}
		return m, ClassImage, nil
	}
	if ext == ".pdf" {
		if sniff != "application/pdf" {
			return "", "", Error(base + " does not look like a PDF")
		}
		return "application/pdf", ClassPDF, nil
	}
	m, ok := textExt[ext]
	if !ok && ext == "" && textNames[base] {
		m, ok = "text/plain", true
	}
	if !ok {
		return "", "", Error("unsupported file type " + ext + " (text, source code, PDF, PNG, JPEG, GIF and WebP are accepted)")
	}
	switch {
	case strings.HasPrefix(sniff, "text/plain"):
	case strings.HasPrefix(sniff, "text/xml") && ext == ".xml":
		if bytes.Contains(bytes.ToLower(data), []byte("<svg")) {
			return "", "", Error("SVG and HTML files are not accepted")
		}
	default:
		return "", "", Error(base + " is not plain text")
	}
	if !utf8.Valid(data) {
		return "", "", Error(base + " is not UTF-8 text")
	}
	if len(data) > MaxTextBytes {
		return "", "", Error("text files are limited to 2 MB")
	}
	return m + "; charset=utf-8", ClassText, nil
}

// ClassOf maps a stored mime type back to its class.
func ClassOf(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return ClassImage
	case mime == "application/pdf":
		return ClassPDF
	}
	return ClassText
}

// Blobs stores bytes at dir/<sha[:2]>/<sha>.
type Blobs struct{ Dir string }

func validSHA(sha string) bool {
	if len(sha) != 64 {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}

// Path is where a blob lives.
func (b Blobs) Path(sha string) (string, error) {
	if !validSHA(sha) {
		return "", errors.New("bad sha256")
	}
	return filepath.Join(b.Dir, sha[:2], sha), nil
}

// Put stores data and returns its sha256; existing blobs are reused.
func (b Blobs) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	p, _ := b.Path(sha)
	if _, err := os.Stat(p); err == nil {
		return sha, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".up-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(f.Name(), 0o600); err != nil {
		return "", err
	}
	return sha, os.Rename(f.Name(), p)
}

// Read returns a blob's bytes.
func (b Blobs) Read(sha string) ([]byte, error) {
	p, err := b.Path(sha)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// Remove deletes a blob; a missing one is not an error.
func (b Blobs) Remove(sha string) error {
	p, err := b.Path(sha)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// MaxPDFText caps extracted text; the request builder refuses longer documents.
const MaxPDFText = 32 << 20

// PDFText extracts the text of a PDF: `pdftotext -layout` when the binary exists,
// github.com/ledongthuc/pdf otherwise. Figures are lost.
func PDFText(ctx context.Context, path string) (string, error) {
	if bin, err := exec.LookPath("pdftotext"); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "-layout", "-enc", "UTF-8", "-q", path, "-")
		var out bytes.Buffer
		cmd.Stdout = &limitWriter{w: &out, n: MaxPDFText}
		if err := cmd.Run(); err == nil {
			return strings.ToValidUTF8(out.String(), "�"), nil
		}
		// fall through: the Go reader may still manage
	}
	return goPDFText(path)
}

func goPDFText(path string) (text string, err error) {
	defer func() {
		if r := recover(); r != nil { // the library panics on some malformed files
			text, err = "", errors.New("could not read the PDF")
		}
	}()
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", errors.New("could not read the PDF")
	}
	defer f.Close()
	rd, err := r.GetPlainText()
	if err != nil {
		return "", errors.New("could not read the PDF")
	}
	b, err := io.ReadAll(io.LimitReader(rd, MaxPDFText))
	if err != nil {
		return "", errors.New("could not read the PDF")
	}
	return strings.ToValidUTF8(string(b), "�"), nil
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	_, err := l.w.Write(q)
	return len(p), err
}
