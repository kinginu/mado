package files

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// MinimalPDF builds a one-page PDF showing text, with a correct xref table.
func MinimalPDF(text string) []byte {
	stream := fmt.Sprintf("BT /F1 24 Tf 72 720 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func pngBytes() []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return b.Bytes()
}

func TestClassify(t *testing.T) {
	ok := []struct {
		name  string
		data  []byte
		mime  string
		class string
	}{
		{"a.png", pngBytes(), "image/png", ClassImage},
		{"doc.pdf", MinimalPDF("hi"), "application/pdf", ClassPDF},
		{"notes.md", []byte("# hi\n"), "text/markdown; charset=utf-8", ClassText},
		{"data.json", []byte(`{"a":1}`), "application/json; charset=utf-8", ClassText},
		{"main.go", []byte("package main\n"), "text/plain; charset=utf-8", ClassText},
		{"x.xml", []byte(`<?xml version="1.0"?><a/>`), "text/plain; charset=utf-8", ClassText},
		{"Makefile", []byte("all:\n"), "text/plain; charset=utf-8", ClassText},
		{"日本語.txt", []byte("こんにちは"), "text/plain; charset=utf-8", ClassText},
	}
	for _, c := range ok {
		m, cl, err := Classify(c.name, c.data)
		if err != nil || m != c.mime || cl != c.class {
			t.Errorf("%s: %q %q %v", c.name, m, cl, err)
		}
	}
	bad := []struct {
		name string
		data []byte
	}{
		{"x.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)},
		{"x.html", []byte("<html></html>")},
		{"x.txt", []byte("<html><script>alert(1)</script></html>")}, // sniffed as HTML
		{"x.xml", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`)},
		{"fake.png", []byte("not an image")},
		{"fake.pdf", []byte("hello")},
		{"x.exe", []byte("MZ")},
		{"bin.txt", []byte{0xff, 0xfe, 0x00, 0x01}},
		{"big.txt", bytes.Repeat([]byte("a"), MaxTextBytes+1)},
		{"png-as.jpg", pngBytes()},
	}
	for _, c := range bad {
		if _, _, err := Classify(c.name, c.data); err == nil {
			t.Errorf("%s accepted", c.name)
		}
	}
}

func TestBlobs(t *testing.T) {
	b := Blobs{Dir: t.TempDir()}
	sha, err := b.Put([]byte("hello"))
	if err != nil || sha != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatal(sha, err)
	}
	if again, _ := b.Put([]byte("hello")); again != sha {
		t.Fatal("not deduplicated")
	}
	p, _ := b.Path(sha)
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 || filepath.Base(filepath.Dir(p)) != "2c" {
		t.Fatal(p, err)
	}
	if got, _ := b.Read(sha); string(got) != "hello" {
		t.Fatal(got)
	}
	if _, err := b.Path("../../etc/passwd"); err == nil {
		t.Fatal("bad sha accepted")
	}
	if b.Remove(sha) != nil || b.Remove(sha) != nil {
		t.Fatal("remove")
	}
}

func TestPDFText(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.pdf")
	os.WriteFile(p, MinimalPDF("Hello mado"), 0o600)
	got, err := goPDFText(p)
	if err != nil || !strings.Contains(got, "Hello") {
		t.Errorf("go reader: %q %v", got, err)
	}
	if _, err := exec.LookPath("pdftotext"); err == nil {
		got, err := PDFText(context.Background(), p)
		if err != nil || !strings.Contains(got, "Hello mado") {
			t.Errorf("pdftotext: %q %v", got, err)
		}
	}
	bad := filepath.Join(t.TempDir(), "b.pdf")
	os.WriteFile(bad, []byte("%PDF-1.4 garbage"), 0o600)
	if _, err := goPDFText(bad); err == nil {
		t.Error("garbage PDF read")
	}
}
