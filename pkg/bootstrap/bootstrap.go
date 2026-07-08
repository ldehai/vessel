// Package bootstrap implements `vessel up`: from a bare Linux machine to a
// running sandbox daemon in one command. It detects KVM, downloads the
// cloud-hypervisor binary and guest kernel, builds a rootfs that embeds the
// running vessel executable as the guest agent, and reports what it did.
//
// Design constraints: no root required, no Docker, no databases. Everything
// lands under ~/.vessel. Machines without KVM degrade to the process driver
// so the API is still usable.
package bootstrap

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	chVersion  = "v52.0"
	chBase     = "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/" + chVersion + "/"
	kernelBase = "https://github.com/cloud-hypervisor/linux/releases/download/ch-release-v6.12.8-20250613/"
)

// Assets are the file paths `vessel up` guarantees to exist on success.
type Assets struct {
	Dir        string // ~/.vessel
	CHBinary   string
	KernelPath string
	RootfsPath string
	KVM        bool // false = process-driver fallback
}

// Progress receives human-readable step updates.
type Progress func(format string, args ...any)

// Up ensures all assets exist and returns them. Idempotent: existing files
// are kept, so the second run is instant.
func Up(report Progress) (*Assets, error) {
	if report == nil {
		report = func(string, ...any) {}
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("vessel up requires Linux (found %s); on macOS run inside a Linux VM", runtime.GOOS)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	a := &Assets{Dir: filepath.Join(home, ".vessel")}
	a.CHBinary = filepath.Join(a.Dir, "bin", "cloud-hypervisor")
	a.KernelPath = filepath.Join(a.Dir, "vmlinux")
	a.RootfsPath = filepath.Join(a.Dir, "rootfs.img")
	if err := os.MkdirAll(filepath.Join(a.Dir, "bin"), 0o755); err != nil {
		return nil, err
	}

	a.KVM = CheckKVM()
	if !a.KVM {
		report("! /dev/kvm not usable — microVMs disabled, falling back to the process driver")
		report("  (fix: enable virtualization in BIOS/hypervisor, then: sudo usermod -aG kvm $USER && re-login)")
		return a, nil
	}
	report("✓ KVM available (%s)", runtime.GOARCH)

	chAsset, kernelAsset, alpineArch := archAssets()
	if chAsset == "" {
		return nil, fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}

	if err := ensureDownload(a.CHBinary, chBase+chAsset, 0o755, report, "cloud-hypervisor "+chVersion); err != nil {
		return nil, err
	}
	if err := ensureDownload(a.KernelPath, kernelBase+kernelAsset, 0o644, report, "guest kernel (CH official)"); err != nil {
		return nil, err
	}

	if _, err := os.Stat(a.RootfsPath); err == nil {
		report("✓ rootfs.img cached")
	} else {
		report("… building rootfs (Alpine + this binary as guest agent)")
		if err := BuildRootfs(a.RootfsPath, alpineArch, report); err != nil {
			return nil, fmt.Errorf("build rootfs: %w", err)
		}
		report("✓ rootfs.img ready")
	}
	return a, nil
}

// CheckKVM reports whether /dev/kvm exists and is read-writable.
func CheckKVM() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func archAssets() (ch, kernel, alpine string) {
	switch runtime.GOARCH {
	case "amd64":
		return "cloud-hypervisor-static", "vmlinux-x86_64", "x86_64"
	case "arm64":
		return "cloud-hypervisor-static-aarch64", "Image-arm64", "aarch64"
	}
	return "", "", ""
}

func ensureDownload(dst, url string, mode os.FileMode, report Progress, label string) error {
	if _, err := os.Stat(dst); err == nil {
		report("✓ %s cached", label)
		return nil
	}
	report("… downloading %s", label)
	tmp := dst + ".tmp"
	if err := download(url, tmp, mode); err != nil {
		return fmt.Errorf("download %s: %w", label, err)
	}
	return os.Rename(tmp, dst)
}

func download(url, dst string, mode os.FileMode) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}
