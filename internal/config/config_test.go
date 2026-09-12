package config

import (
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("the default configuration does not validate: %v", err)
	}
}

// Loopback by default: TLS and SSO belong to a proxy in front (design
// section 10).
func TestDefaultBindsLoopback(t *testing.T) {
	if got := Default().Listen; !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("default listen = %q, want a loopback address", got)
	}
}

func TestBindParsesFlags(t *testing.T) {
	c := Default()
	fs := flag.NewFlagSet("labview", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	Bind(fs, &c)

	err := fs.Parse([]string{
		"-listen", "127.0.0.1:9999",
		"-inventory", "/etc/labview/machines",
		"-lease-idle", "90s",
		"-scrollback-bytes", "1024",
		"-tile-mode", "screenshot",
		"-host-access", "fake",
		"-recordings-dir", "/tmp/casts",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if c.Listen != "127.0.0.1:9999" || c.InventoryDir != "/etc/labview/machines" {
		t.Errorf("config = %+v", c)
	}
	if c.LeaseIdle != 90*time.Second {
		t.Errorf("LeaseIdle = %s", c.LeaseIdle)
	}
	if c.RingBytes != 1024 {
		t.Errorf("RingBytes = %d", c.RingBytes)
	}
	if c.TileMode != TileScreenshot {
		t.Errorf("TileMode = %q", c.TileMode)
	}
	if c.HostAccess != HostFake {
		t.Errorf("HostAccess = %q", c.HostAccess)
	}
}

func TestBindRejectsUnknownEnums(t *testing.T) {
	for _, args := range [][]string{
		{"-tile-mode", "hologram"},
		{"-host-access", "libvirt"},
	} {
		c := Default()
		fs := flag.NewFlagSet("labview", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		Bind(fs, &c)
		if err := fs.Parse(args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestValidateCatchesBadCombinations(t *testing.T) {
	cases := map[string]func(*Config){
		"no listen address":      func(c *Config) { c.Listen = "" },
		"no inventory":           func(c *Config) { c.InventoryDir = "" },
		"no identity header":     func(c *Config) { c.IdentityHeader = "" },
		"no default identity":    func(c *Config) { c.DefaultIdentity = "" },
		"zero lease":             func(c *Config) { c.LeaseIdle = 0 },
		"warning outlasts lease": func(c *Config) { c.LeaseWarn = c.LeaseIdle },
		"zero scrollback":        func(c *Config) { c.RingBytes = 0 },
	}
	for name, mutate := range cases {
		c := Default()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// Retention settings with capture disabled are pointless rather than
// dangerous, but saying so beats ignoring them.
func TestValidateFlagsRetentionWithoutCapture(t *testing.T) {
	c := Default()
	c.TranscriptDir = ""
	err := c.Validate()
	if err == nil {
		t.Fatal("retention with no recordings directory was accepted silently")
	}
	if !strings.Contains(err.Error(), "recordings-dir") {
		t.Errorf("unhelpful error: %v", err)
	}

	// With retention also off, an empty directory is a legitimate choice.
	c.TranscriptMaxFiles = 0
	if err := c.Validate(); err != nil {
		t.Errorf("disabling capture entirely should be valid: %v", err)
	}
}
