package lifecycle

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
)

func TestCreateAndDestroy(t *testing.T) {
	root := t.TempDir()
	invDir := filepath.Join(root, "inventory.d")
	machDir := filepath.Join(root, "machines")
	if err := os.MkdirAll(invDir, 0o755); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := inventory.NewWatcher(invDir, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })

	mgr := New(Options{
		MachinesDir:  machDir,
		InventoryDir: invDir,
		Inventory:    w,
		Log:          log,
	})

	spec := Spec{ID: "el9-build", Name: "el9-build", MemoryMB: 1024, VCPUs: 2, DiskGB: 1}
	mach, err := mgr.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if mach.Unit != "qemu-vm@el9-build.service" {
		t.Errorf("unit = %q", mach.Unit)
	}
	if !mgr.Managed("el9-build") {
		t.Error("created machine is not managed")
	}
	if _, err := os.Stat(filepath.Join(machDir, "el9-build", "disk.raw")); err != nil {
		t.Fatalf("disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(invDir, "el9-build.yaml")); err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if got, ok := w.Current().Get("el9-build"); !ok || !got.CanPower() {
		t.Fatalf("watcher missing machine: ok=%v got=%+v", ok, got)
	}

	if _, err := mgr.Create(context.Background(), spec); err == nil {
		t.Fatal("duplicate create succeeded")
	}

	if err := mgr.Destroy(context.Background(), "el9-build"); err != nil {
		t.Fatal(err)
	}
	if mgr.Managed("el9-build") {
		t.Error("destroyed machine still managed")
	}
	if _, ok := w.Current().Get("el9-build"); ok {
		t.Error("watcher still holds destroyed machine")
	}
}

func TestCreateValidation(t *testing.T) {
	mgr := New(Options{})
	if _, err := mgr.Create(context.Background(), Spec{ID: "x"}); err != ErrDisabled {
		t.Fatalf("disabled: %v", err)
	}

	spec := Spec{ID: "bad id", MemoryMB: 1, VCPUs: 1, DiskGB: 1}
	if err := spec.Validate(); err == nil {
		t.Fatal("invalid id accepted")
	}
}

func TestSpecUnitName(t *testing.T) {
	s := Spec{ID: "vm1"}
	if s.UnitName() != "qemu-vm@vm1.service" {
		t.Fatal(s.UnitName())
	}
}

func TestCreateMultiDiskAndPatch(t *testing.T) {
	root := t.TempDir()
	invDir := filepath.Join(root, "inventory.d")
	machDir := filepath.Join(root, "machines")
	fwDir := filepath.Join(root, "fw")
	if err := os.MkdirAll(invDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fwDir, 0o755); err != nil {
		t.Fatal(err)
	}
	code := filepath.Join(fwDir, "CODE.fd")
	vars := filepath.Join(fwDir, "VARS.fd")
	if err := createSparseDisk(code, 1024); err != nil {
		t.Fatal(err)
	}
	if err := createSparseDisk(vars, 1024); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := inventory.NewWatcher(invDir, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })

	mgr := New(Options{
		MachinesDir:  machDir,
		InventoryDir: invDir,
		Inventory:    w,
		Firmware:     FirmwarePaths{Code: code, VarsTemplate: vars},
		Log:          log,
	})

	spec := Spec{
		ID: "hw1", MemoryMB: 1024, VCPUs: 2,
		Firmware: "uefi",
		Disks: []DiskSpec{
			{Name: "root", SizeGB: 1},
			{Name: "data", SizeGB: 2, Bus: "scsi"},
		},
		TPM:  &TPMSpec{},
		CCID: []CCIDSpec{{ID: "reader0"}},
	}
	if _, err := mgr.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"root.raw", "data.raw", "OVMF_VARS.fd", "qemu.argv", "spec.json"} {
		if _, err := os.Stat(filepath.Join(machDir, "hw1", name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(machDir, "hw1", "tpm")); err != nil {
		t.Fatalf("tpm dir: %v", err)
	}

	mb := int64(2048)
	res, err := mgr.Patch(context.Background(), "hw1", PatchSpec{MemoryMB: &mb})
	if err != nil {
		t.Fatal(err)
	}
	if !res.NeedsRestart || res.Spec.MemoryMB != 2048 {
		t.Fatalf("%+v", res)
	}
	desired, err := mgr.Desired("hw1")
	if err != nil {
		t.Fatal(err)
	}
	if desired.MemoryMB != 2048 || desired.FirmwareNorm() != FirmwareUEFI {
		t.Fatalf("%+v", desired)
	}
}
