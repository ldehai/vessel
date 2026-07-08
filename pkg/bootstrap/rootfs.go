package bootstrap

import (
	"archive/tar"
	"compress/gzip"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const alpineVersion = "3.20.3"

// initScript mounts pseudo-filesystems and becomes the guest agent. The
// vessel binary doubles as the agent via the `agent` subcommand, so the
// rootfs needs no separately compiled artifact.
const initScript = `#!/bin/sh
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mount -t proc     proc     /proc  2>/dev/null
mount -t sysfs    sysfs    /sys   2>/dev/null
mount -t devtmpfs devtmpfs /dev   2>/dev/null
mount -t tmpfs    tmpfs    /tmp   2>/dev/null
hostname vessel-guest 2>/dev/null
exec /usr/bin/vessel agent
`

// BuildRootfs assembles an ext4 guest image at dst: Alpine minirootfs plus
// the currently running vessel executable installed as /usr/bin/vessel and
// wired up as init. Pure Go except the final mkfs.ext4 -d (e2fsprogs).
func BuildRootfs(dst, alpineArch string, report Progress) error {
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return fmt.Errorf("mkfs.ext4 not found (install e2fsprogs: sudo apt install e2fsprogs)")
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if static, err := isStaticELF(self); err == nil && !static {
		report("! warning: %s is dynamically linked; the guest (musl) cannot run it.", filepath.Base(self))
		report("  rebuild with: CGO_ENABLED=0 go build ./cmd/vessel")
	}

	work, err := os.MkdirTemp("", "vessel-rootfs-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	root := filepath.Join(work, "root")

	url := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v%s/releases/%s/alpine-minirootfs-%s-%s.tar.gz",
		alpineVersion[:strings.LastIndex(alpineVersion, ".")], alpineArch, alpineVersion, alpineArch)
	report("  fetching Alpine minirootfs %s (%s)", alpineVersion, alpineArch)
	tarball := filepath.Join(work, "alpine.tar.gz")
	if err := download(url, tarball, 0o644); err != nil {
		return fmt.Errorf("fetch alpine: %w", err)
	}
	if err := extractTarGz(tarball, root); err != nil {
		return fmt.Errorf("extract alpine: %w", err)
	}

	// Install this binary as the guest agent + init.
	if err := copyFile(self, filepath.Join(root, "usr/bin/vessel"), 0o755); err != nil {
		return err
	}
	initPath := filepath.Join(root, "sbin/init")
	_ = os.Remove(initPath) // Alpine ships /sbin/init as an absolute symlink
	if err := os.WriteFile(initPath, []byte(initScript), 0o755); err != nil {
		return err
	}

	report("  packing ext4 image")
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := f.Truncate(192 << 20); err != nil { // 192MiB: alpine+binary with headroom
		f.Close()
		return err
	}
	f.Close()
	if out, err := exec.Command(mkfs, "-q", "-F", "-d", root, tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4: %v: %s", err, out)
	}
	return os.Rename(tmp, dst)
}

// extractTarGz unpacks dirs, regular files, symlinks and hardlinks. Device
// nodes and other special entries are skipped (non-root safe); Alpine's
// minirootfs does not rely on them.
func extractTarGz(tarball, dst string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") {
			continue // path traversal guard
		}
		target := filepath.Join(dst, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, tr); err != nil { //nolint:gosec // trusted alpine tarball
				w.Close()
				return err
			}
			w.Close()
		case tar.TypeSymlink:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			_ = os.Remove(target)
			if err := os.Link(filepath.Join(dst, filepath.Clean(hdr.Linkname)), target); err != nil {
				return err
			}
		default:
			// char/block devices, fifos: skip (created by devtmpfs at boot)
		}
	}
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// isStaticELF reports whether path is a statically linked ELF binary.
func isStaticELF(path string) (bool, error) {
	f, err := elf.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return false, nil
		}
	}
	return true, nil
}
