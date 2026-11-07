package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
)

// FirmwarePaths are the OVMF files on the node image.
type FirmwarePaths struct {
	// Code is the read-only OVMF_CODE.fd path.
	Code string
	// VarsTemplate is copied into the machine directory on create.
	VarsTemplate string
}

// DefaultFirmwarePaths matches Fedora edk2-ovmf layout.
func DefaultFirmwarePaths() FirmwarePaths {
	return FirmwarePaths{
		Code:         "/usr/share/edk2/ovmf/OVMF_CODE.fd",
		VarsTemplate: "/usr/share/edk2/ovmf/OVMF_VARS.fd",
	}
}

// buildArgv returns one QEMU argument per line for systemd @file form.
func buildArgv(spec Spec, dir string, serial, qmp string, vncDisplay int, fw FirmwarePaths) ([]string, error) {
	disks, err := spec.ResolvedDisks()
	if err != nil {
		return nil, err
	}

	lines := []string{
		"-name", spec.ID,
		"-machine", "q35,accel=kvm:tcg",
		"-cpu", "max",
		"-m", fmt.Sprintf("%d", spec.MemoryMB),
		"-smp", fmt.Sprintf("%d", spec.VCPUs),
		"-display", "none",
		"-vnc", fmt.Sprintf("127.0.0.1:%d", vncDisplay),
		"-chardev", fmt.Sprintf("socket,id=serial0,path=%s,server=on,wait=off", serial),
		"-serial", "chardev:serial0",
		"-qmp", fmt.Sprintf("unix:%s,server=on,wait=off", qmp),
		"-nodefaults",
	}

	if spec.FirmwareNorm() == FirmwareUEFI {
		vars := filepath.Join(dir, "OVMF_VARS.fd")
		lines = append(lines,
			"-drive", fmt.Sprintf("if=pflash,format=raw,readonly=on,file=%s", fw.Code),
			"-drive", fmt.Sprintf("if=pflash,format=raw,file=%s", vars),
		)
	}

	needSCSI := false
	for _, d := range disks {
		if d.BusNorm() == "scsi" {
			needSCSI = true
			break
		}
	}
	if needSCSI {
		lines = append(lines, "-device", "virtio-scsi-pci,id=scsi0")
	}

	for i, d := range disks {
		path := diskPath(dir, d)
		ro := ""
		if d.ReadOnly {
			ro = ",readonly=on"
		}
		switch d.BusNorm() {
		case "virtio":
			lines = append(lines, "-drive",
				fmt.Sprintf("file=%s,format=raw,if=virtio,cache=writeback%s", path, ro))
		case "scsi":
			driveID := fmt.Sprintf("drive-%s", d.Name)
			lines = append(lines,
				"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=%s,cache=writeback%s", path, driveID, ro),
				"-device", fmt.Sprintf("scsi-hd,drive=%s,bus=scsi0.0,scsi-id=%d", driveID, i),
			)
		case "ide":
			lines = append(lines, "-drive",
				fmt.Sprintf("file=%s,format=raw,if=ide,cache=writeback%s", path, ro))
		}
	}

	nUSB := spec.USBControllers()
	for i := 0; i < nUSB; i++ {
		lines = append(lines, "-device", fmt.Sprintf("qemu-xhci,id=xhci%d", i))
	}
	if spec.USB != nil {
		for _, d := range spec.USB.Devices {
			switch d.Kind {
			case "tablet":
				lines = append(lines, "-device", fmt.Sprintf("usb-tablet,id=%s,bus=xhci0.0", d.ID))
			case "storage":
				driveID := "usbdrive-" + d.ID
				lines = append(lines,
					"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=%s", d.Image, driveID),
					"-device", fmt.Sprintf("usb-storage,id=%s,drive=%s,bus=xhci0.0", d.ID, driveID),
				)
			}
		}
	}
	for _, c := range spec.CCID {
		lines = append(lines, "-device", fmt.Sprintf("usb-ccid,id=%s,bus=xhci0.0", c.ID))
	}

	if spec.TPM != nil {
		sock := filepath.Join(dir, "swtpm.sock")
		lines = append(lines,
			"-chardev", fmt.Sprintf("socket,id=chrtpm,path=%s", sock),
			"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
			"-device", fmt.Sprintf("%s,tpmdev=tpm0", spec.TPM.ModelNorm()),
		)
	}

	return lines, nil
}

func writeArgv(path string, lines []string) error {
	var b []byte
	for _, line := range lines {
		b = append(b, line...)
		b = append(b, '\n')
	}
	return os.WriteFile(path, b, 0o640)
}

func diskPath(dir string, d DiskSpec) string {
	if d.Image != "" {
		return d.Image
	}
	return filepath.Join(dir, d.Name+".raw")
}

func prepareUEFIVars(dir string, fw FirmwarePaths) error {
	dst := filepath.Join(dir, "OVMF_VARS.fd")
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	in, err := os.ReadFile(fw.VarsTemplate)
	if err != nil {
		return fmt.Errorf("read OVMF vars template %s: %w", fw.VarsTemplate, err)
	}
	if err := os.WriteFile(dst, in, 0o640); err != nil {
		return fmt.Errorf("write OVMF vars: %w", err)
	}
	return nil
}

func prepareDisks(dir string, disks []DiskSpec) error {
	for _, d := range disks {
		if d.Image != "" {
			if _, err := os.Stat(d.Image); err != nil {
				return fmt.Errorf("disk %q image %s: %w", d.Name, d.Image, err)
			}
			continue
		}
		path := filepath.Join(dir, d.Name+".raw")
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := createSparseDisk(path, d.SizeGB<<30); err != nil {
			return fmt.Errorf("disk %q: %w", d.Name, err)
		}
	}
	return nil
}

func prepareTPMState(dir string) error {
	return os.MkdirAll(filepath.Join(dir, "tpm"), 0o750)
}
