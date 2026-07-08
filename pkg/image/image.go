// Package image converts an on-disk directory tree (an OCI bundle's
// unpacked rootfs) into a block image a microVM can mount as virtio-blk.
//
// containerd hands a runtime the rootfs as a directory; the process driver
// consumes it directly, but Cloud Hypervisor needs a block device. This
// package bridges that gap without root: mkfs.erofs (read-only, dedup,
// page-cache shared across VMs — the production choice) when available,
// falling back to mkfs.ext4 -d (e2fsprogs, no privileges) otherwise.
package image

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Format is the on-disk filesystem of a packed image.
type Format string

const (
	FormatErofs Format = "erofs"
	FormatExt4  Format = "ext4"
)

// Options tune packing.
type Options struct {
	// Format forces a filesystem. Empty auto-selects: erofs if mkfs.erofs is
	// on PATH, else ext4.
	Format Format
	// ExtraMiB is headroom added to the estimated size for ext4 (which needs
	// a pre-sized image). Ignored for erofs (grows to fit). Default 32.
	ExtraMiB int64
}

// Result describes a packed image.
type Result struct {
	Path   string
	Format Format
	Bytes  int64
}

// PackDir writes srcDir's tree into a block image at dstImage and returns
// what it built. dstImage is replaced if it exists.
func PackDir(srcDir, dstImage string, opts Options) (*Result, error) {
	info, err := os.Stat(srcDir)
	if err != nil {
		return nil, fmt.Errorf("source rootfs: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source rootfs %s is not a directory", srcDir)
	}

	format := opts.Format
	if format == "" {
		if _, err := exec.LookPath("mkfs.erofs"); err == nil {
			format = FormatErofs
		} else {
			format = FormatExt4
		}
	}

	if err := os.MkdirAll(filepath.Dir(dstImage), 0o755); err != nil {
		return nil, err
	}
	tmp := dstImage + ".tmp"
	_ = os.Remove(tmp)

	switch format {
	case FormatErofs:
		if err := packErofs(srcDir, tmp); err != nil {
			return nil, err
		}
	case FormatExt4:
		if err := packExt4(srcDir, tmp, opts.ExtraMiB); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown image format %q", format)
	}

	if err := os.Rename(tmp, dstImage); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	st, _ := os.Stat(dstImage)
	return &Result{Path: dstImage, Format: format, Bytes: st.Size()}, nil
}

func packErofs(srcDir, dst string) error {
	// mkfs.erofs <image> <dir>; grows to fit, no pre-sizing needed.
	if out, err := run("mkfs.erofs", "-zlz4", dst, srcDir); err != nil {
		return fmt.Errorf("mkfs.erofs: %w: %s", err, out)
	}
	return nil
}

func packExt4(srcDir, dst string, extraMiB int64) error {
	if extraMiB <= 0 {
		extraMiB = 32
	}
	used, err := dirSizeBytes(srcDir)
	if err != nil {
		return err
	}
	// ext4 needs a pre-sized image: content + headroom for metadata/inodes,
	// floored so tiny rootfs still format cleanly.
	sizeMiB := used/(1<<20) + extraMiB + 16
	if sizeMiB < 32 {
		sizeMiB = 32
	}
	if err := truncate(dst, sizeMiB<<20); err != nil {
		return err
	}
	// -d populates from a directory; -F forces on a regular file; no root.
	if out, err := run("mkfs.ext4", "-q", "-F", "-d", srcDir, dst); err != nil {
		return fmt.Errorf("mkfs.ext4: %w: %s", err, out)
	}
	return nil
}

func dirSizeBytes(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func truncate(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
