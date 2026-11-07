package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
	"gopkg.in/yaml.v3"
)

func writeInventoryFile(dir string, m inventory.Machine) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("inventory dir: %w", err)
	}
	doc := map[string]any{
		"name":    m.Name,
		"vnc":     m.VNC,
		"serial":  m.Serial,
		"control": m.Control,
		"unit":    m.Unit,
	}
	if m.Notes != "" {
		doc["notes"] = m.Notes
	}
	if m.Host != "" {
		doc["host"] = m.Host
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, m.ID+".yaml")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeInventoryFile(dir, id string) error {
	err := os.Remove(filepath.Join(dir, id+".yaml"))
	if os.IsNotExist(err) {
		err = os.Remove(filepath.Join(dir, id+".yml"))
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
