package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/maglo/qemu-lab-manager/labview/internal/lifecycle"
)

func enableLifecycle(t *testing.T, h *harness) {
	t.Helper()
	machDir := filepath.Join(t.TempDir(), "machines")
	if err := os.MkdirAll(machDir, 0o750); err != nil {
		t.Fatal(err)
	}
	h.srv.life = lifecycle.New(lifecycle.Options{
		MachinesDir:  machDir,
		InventoryDir: h.srv.cfg.InventoryDir,
		Inventory:    h.srv.inventory,
		Log:          h.srv.log,
	})
}

func TestCreateAndDestroyMachine(t *testing.T) {
	h := newHarness(t)
	enableLifecycle(t, h)

	body := map[string]any{
		"id": "api-vm", "name": "api-vm",
		"memoryMb": 512, "vcpus": 1, "diskGb": 1,
	}
	rec := h.do("POST", "/api/machines", "alice", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "api-vm" {
		t.Fatalf("id = %q", created.ID)
	}

	rec = h.do("DELETE", "/api/machines/api-vm", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("destroy status = %d: %s", rec.Code, rec.Body)
	}
}

func TestDestroyRequiresLease(t *testing.T) {
	h := newHarness(t)
	enableLifecycle(t, h)

	rec := h.do("POST", "/api/machines", "alice", map[string]any{
		"id": "leased", "memoryMb": 512, "vcpus": 1, "diskGb": 1,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}

	rec = h.do("DELETE", "/api/machines/leased", "bob", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("bob destroy status = %d: %s", rec.Code, rec.Body)
	}
}

func TestCreateDisabled(t *testing.T) {
	h := newHarness(t)
	h.srv.life = nil
	rec := h.do("POST", "/api/machines", "alice", map[string]any{
		"id": "x", "memoryMb": 1, "vcpus": 1, "diskGb": 1,
	})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}

func TestCreateRejectsBadJSON(t *testing.T) {
	h := newHarness(t)
	enableLifecycle(t, h)
	req := httptest.NewRequest("POST", "/api/machines", bytes.NewReader([]byte("{")))
	req.Header.Set("X-Forwarded-User", "alice")
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}
