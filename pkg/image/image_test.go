package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// makeRootfs builds a small directory tree standing in for an unpacked OCI
// rootfs.
func makeRootfs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"bin", "etc", "usr/bin"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "etc/hostname"), []byte("vessel-guest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin/true"), make([]byte, 4096), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPackDirExt4(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}
	src := makeRootfs(t)
	dst := filepath.Join(t.TempDir(), "rootfs.img")

	res, err := PackDir(src, dst, Options{Format: FormatExt4})
	if err != nil {
		t.Fatal(err)
	}
	if res.Format != FormatExt4 {
		t.Fatalf("format = %s, want ext4", res.Format)
	}
	if res.Path != dst {
		t.Fatalf("path = %s, want %s", res.Path, dst)
	}
	// An ext4 image begins with 1024 bytes of padding then the superblock
	// magic 0x53 0xEF at offset 0x438.
	f, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	magic := make([]byte, 2)
	if _, err := f.ReadAt(magic, 0x438); err != nil {
		t.Fatal(err)
	}
	if magic[0] != 0x53 || magic[1] != 0xEF {
		t.Fatalf("ext4 superblock magic = %x, want 53ef", magic)
	}
	if res.Bytes < 32<<20 {
		t.Fatalf("image %d bytes, want >= 32MiB floor", res.Bytes)
	}
}

func TestPackDirAutoSelectsAvailableFormat(t *testing.T) {
	src := makeRootfs(t)
	dst := filepath.Join(t.TempDir(), "auto.img")

	res, err := PackDir(src, dst, Options{}) // no explicit format
	if err != nil {
		if _, e := exec.LookPath("mkfs.ext4"); e != nil {
			t.Skip("no mkfs tools available")
		}
		t.Fatal(err)
	}
	// Whatever it picked must actually exist on PATH.
	if _, err := exec.LookPath("mkfs." + string(res.Format)); err != nil {
		t.Fatalf("auto-selected %s but mkfs.%s is not on PATH", res.Format, res.Format)
	}
}

func TestPackDirReplacesExisting(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}
	src := makeRootfs(t)
	dst := filepath.Join(t.TempDir(), "rootfs.img")
	// Pre-existing junk at the destination must be overwritten atomically.
	if err := os.WriteFile(dst, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PackDir(src, dst, Options{Format: FormatExt4}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp image left behind")
	}
}

func TestPackDirRejectsNonDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if _, err := PackDir(f, filepath.Join(t.TempDir(), "out.img"), Options{}); err == nil {
		t.Fatal("packing a non-directory must error")
	}
	if _, err := PackDir("/nonexistent", filepath.Join(t.TempDir(), "out.img"), Options{}); err == nil {
		t.Fatal("packing a missing source must error")
	}
}

func TestPackDirUnknownFormat(t *testing.T) {
	src := makeRootfs(t)
	if _, err := PackDir(src, filepath.Join(t.TempDir(), "x.img"), Options{Format: "squashfs"}); err == nil {
		t.Fatal("unknown format must error")
	}
}
