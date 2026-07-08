package bootstrap

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTarGz builds a synthetic tarball exercising every entry type the
// extractor must handle (plus ones it must skip).
func makeTarGz(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.tar.gz")
	f, _ := os.Create(path)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	add := func(hdr *tar.Header, body []byte) {
		hdr.Size = int64(len(body))
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if body != nil {
			_, _ = tw.Write(body)
		}
	}
	add(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755}, nil)
	add(&tar.Header{Name: "bin/busybox", Typeflag: tar.TypeReg, Mode: 0o755}, []byte("#!fake"))
	add(&tar.Header{Name: "bin/sh", Typeflag: tar.TypeSymlink, Linkname: "/bin/busybox", Mode: 0o777}, nil)
	add(&tar.Header{Name: "bin/ash", Typeflag: tar.TypeLink, Linkname: "bin/busybox", Mode: 0o755}, nil)
	add(&tar.Header{Name: "dev/null", Typeflag: tar.TypeChar, Mode: 0o666}, nil)         // must be skipped
	add(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644}, []byte("x")) // traversal guard

	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()
	return path
}

func TestExtractTarGz(t *testing.T) {
	dst := t.TempDir()
	if err := extractTarGz(makeTarGz(t), dst); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "bin/busybox"))
	if err != nil || string(data) != "#!fake" {
		t.Fatalf("busybox: %q %v", data, err)
	}
	link, err := os.Readlink(filepath.Join(dst, "bin/sh"))
	if err != nil || link != "/bin/busybox" {
		t.Fatalf("symlink: %q %v", link, err)
	}
	a, _ := os.Stat(filepath.Join(dst, "bin/busybox"))
	b, err := os.Stat(filepath.Join(dst, "bin/ash"))
	if err != nil || !os.SameFile(a, b) {
		t.Fatal("hardlink not preserved")
	}
	if _, err := os.Stat(filepath.Join(dst, "dev/null")); !os.IsNotExist(err) {
		t.Fatal("device node should be skipped")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "escape")); !os.IsNotExist(err) {
		t.Fatal("path traversal not blocked")
	}
}

func TestIsStaticELFRejectsNonELF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "not-elf")
	_ = os.WriteFile(p, []byte("plain text"), 0o644)
	if _, err := isStaticELF(p); err == nil {
		t.Fatal("want error for non-ELF file")
	}
}

func TestArchAssets(t *testing.T) {
	ch, kernel, alpine := archAssets()
	if ch == "" || kernel == "" || alpine == "" {
		t.Fatalf("archAssets on %s returned empty", "current arch")
	}
}

func TestInitScriptExecsAgent(t *testing.T) {
	if !strings.Contains(initScript, "exec /usr/bin/vessel agent") {
		t.Fatal("init must exec the embedded vessel binary in agent mode")
	}
}
