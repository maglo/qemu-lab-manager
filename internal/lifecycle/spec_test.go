package lifecycle

import (
	"strings"
	"testing"
)

func TestSpecValidateShorthandAndDisks(t *testing.T) {
	if err := (Spec{ID: "a", MemoryMB: 512, VCPUs: 1}).Validate(); err == nil {
		t.Fatal("missing disk accepted")
	}
	if err := (Spec{ID: "a", MemoryMB: 512, VCPUs: 1, DiskGB: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Spec{
		ID: "a", MemoryMB: 512, VCPUs: 1,
		Disks:    []DiskSpec{{Name: "root", SizeGB: 2}, {Name: "data", SizeGB: 4, Bus: "scsi"}},
		Firmware: "uefi",
		TPM:      &TPMSpec{},
		CCID:     []CCIDSpec{{ID: "reader0"}},
		USB:      &USBSpec{Devices: []USBDeviceSpec{{ID: "tab", Kind: "tablet"}}},
	}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSpecApplyPatch(t *testing.T) {
	s := Spec{ID: "a", MemoryMB: 512, VCPUs: 1, DiskGB: 1, Firmware: "bios"}
	mb := int64(2048)
	fw := "uefi"
	next := s.Apply(PatchSpec{MemoryMB: &mb, Firmware: &fw})
	if next.MemoryMB != 2048 || next.FirmwareNorm() != FirmwareUEFI {
		t.Fatalf("%+v", next)
	}
	next = next.Apply(PatchSpec{ClearTPM: true, TPM: &TPMSpec{Version: "2.0"}})
	if next.TPM != nil {
		t.Fatal("clearTpm should win when both set? actually Apply checks ClearTPM first")
	}
	tpm := &TPMSpec{Model: "tpm-crb"}
	next = s.Apply(PatchSpec{TPM: tpm})
	if next.TPM == nil || next.TPM.ModelNorm() != "tpm-crb" {
		t.Fatalf("%+v", next.TPM)
	}
}

func TestBuildArgvUEFIAndTPM(t *testing.T) {
	dir := t.TempDir()
	fwCode := dir + "/CODE.fd"
	fwVars := dir + "/VARS.fd"
	if err := createSparseDisk(fwCode, 1024); err != nil {
		t.Fatal(err)
	}
	if err := createSparseDisk(fwVars, 1024); err != nil {
		t.Fatal(err)
	}
	spec := Spec{
		ID: "vm1", MemoryMB: 1024, VCPUs: 2, DiskGB: 1,
		Firmware: "uefi",
		TPM:      &TPMSpec{},
		CCID:     []CCIDSpec{{ID: "r0"}},
	}
	if err := prepareDisks(dir, mustDisks(t, spec)); err != nil {
		t.Fatal(err)
	}
	if err := prepareUEFIVars(dir, FirmwarePaths{Code: fwCode, VarsTemplate: fwVars}); err != nil {
		t.Fatal(err)
	}
	if err := prepareTPMState(dir); err != nil {
		t.Fatal(err)
	}
	lines, err := buildArgv(spec, dir, dir+"/s.sock", dir+"/q.sock", 3,
		FirmwarePaths{Code: fwCode, VarsTemplate: fwVars})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"pflash", "tpm-tis", "usb-ccid", "qemu-xhci", "swtpm.sock"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

func mustDisks(t *testing.T, s Spec) []DiskSpec {
	t.Helper()
	d, err := s.ResolvedDisks()
	if err != nil {
		t.Fatal(err)
	}
	return d
}
