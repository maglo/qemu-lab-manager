// Package config holds labview's settings.
//
// Everything is a flag with a sensible default, because labview is one binary
// with one inventory directory and no database (design, preamble) -- adding a
// config file format would be a third interface to maintain alongside the
// inventory and the API.
package config

import (
	"errors"
	"flag"
	"fmt"
	"time"
)

// TileMode selects how the wall renders a tile.
//
// Beyond roughly eight tiles, every live RFB session is a decoder running in
// one browser tab, so the wall should switch to periodic screenshots and open
// RFB only on click (design section 7). The tile is built as a component that
// can render either, which is what makes this a config change.
type TileMode string

const (
	TileRFB        TileMode = "rfb"
	TileScreenshot TileMode = "screenshot"
)

// HostAccessMode selects the host access implementation.
type HostAccessMode string

const (
	// HostLocal reads and controls the hypervisor labview runs on.
	HostLocal HostAccessMode = "local"
	// HostNone serves the wall with no host introspection at all, for
	// running labview somewhere other than a hypervisor.
	HostNone HostAccessMode = "none"
	// HostFake invents plausible details, for working on the UI without a
	// lab. Never a fallback: it is selected explicitly, because a wall
	// silently showing invented details would be worse than one saying it
	// cannot reach the host.
	HostFake HostAccessMode = "fake"
)

// Config is labview's full configuration.
type Config struct {
	// Listen is the address to serve on. Loopback by default: a reverse
	// proxy terminates TLS and does SSO in front (design section 10).
	Listen string

	// InventoryDir holds one YAML file per machine.
	InventoryDir    string
	InventoryRescan time.Duration

	// IdentityHeader carries the identity the proxy asserted. It names the
	// lease holder in the UI and says who attached to what in the log. It
	// is not an authorisation input -- everyone who gets past the proxy can
	// see every machine (design section 10).
	IdentityHeader string
	// DefaultIdentity is used when the header is absent, which in
	// production means the proxy is misconfigured.
	DefaultIdentity string

	LeaseIdle  time.Duration
	LeaseWarn  time.Duration
	LeaseSweep time.Duration

	RingBytes       int
	SubscriberQueue int
	DialTimeout     time.Duration
	BackoffMin      time.Duration
	BackoffMax      time.Duration

	TranscriptDir        string
	TranscriptMaxFiles   int
	TranscriptMaxAge     time.Duration
	TranscriptMaxBytes   int64
	TranscriptMaxRunSize int64

	TileMode   TileMode
	HostAccess HostAccessMode

	ActivityCapacity int

	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	Verbose bool
}

// Default returns the configuration labview runs with if nothing is set.
func Default() Config {
	return Config{
		Listen:          "127.0.0.1:8080",
		InventoryDir:    "/etc/labview/inventory.d",
		InventoryRescan: 2 * time.Second,
		IdentityHeader:  "X-Forwarded-User",
		DefaultIdentity: "unidentified",

		LeaseIdle:  3 * time.Minute,
		LeaseWarn:  30 * time.Second,
		LeaseSweep: 2 * time.Second,

		RingBytes:       256 << 10,
		SubscriberQueue: 256,
		DialTimeout:     5 * time.Second,
		BackoffMin:      250 * time.Millisecond,
		BackoffMax:      5 * time.Second,

		TranscriptDir:        "/var/lib/labview/recordings",
		TranscriptMaxFiles:   20,
		TranscriptMaxAge:     14 * 24 * time.Hour,
		TranscriptMaxBytes:   512 << 20,
		TranscriptMaxRunSize: 64 << 20,

		TileMode:   TileRFB,
		HostAccess: HostLocal,

		ActivityCapacity: 200,

		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
}

// Bind registers flags for every setting on fs.
func Bind(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.Listen, "listen", c.Listen,
		"address to serve on; loopback by default, with TLS and SSO in a proxy in front")
	fs.StringVar(&c.InventoryDir, "inventory", c.InventoryDir,
		"directory holding one YAML file per machine, watched for changes")
	fs.DurationVar(&c.InventoryRescan, "inventory-rescan", c.InventoryRescan,
		"how often to read the inventory directory again, as a backstop to the watch")

	fs.StringVar(&c.IdentityHeader, "identity-header", c.IdentityHeader,
		"request header carrying the identity asserted by the proxy")
	fs.StringVar(&c.DefaultIdentity, "default-identity", c.DefaultIdentity,
		"identity to use when the header is absent")

	fs.DurationVar(&c.LeaseIdle, "lease-idle", c.LeaseIdle,
		"how long a write lease survives with no input")
	fs.DurationVar(&c.LeaseWarn, "lease-warn", c.LeaseWarn,
		"how long before expiry to warn the lease holder")

	fs.IntVar(&c.RingBytes, "scrollback-bytes", c.RingBytes,
		"serial scrollback held per machine and replayed on attach")
	fs.IntVar(&c.SubscriberQueue, "subscriber-queue", c.SubscriberQueue,
		"chunks a serial subscriber may fall behind by before it is dropped")

	fs.StringVar(&c.TranscriptDir, "recordings-dir", c.TranscriptDir,
		"directory for serial captures; empty disables capture")
	fs.IntVar(&c.TranscriptMaxFiles, "recordings-max-files", c.TranscriptMaxFiles,
		"captures to retain per machine")
	fs.DurationVar(&c.TranscriptMaxAge, "recordings-max-age", c.TranscriptMaxAge,
		"how long to retain a capture")
	fs.Int64Var(&c.TranscriptMaxBytes, "recordings-max-bytes", c.TranscriptMaxBytes,
		"total capture bytes to retain per machine")
	fs.Int64Var(&c.TranscriptMaxRunSize, "recordings-max-run-bytes", c.TranscriptMaxRunSize,
		"size ceiling for a single capture, as a backstop against a machine that spews")

	fs.Func("tile-mode", "wall tile renderer: rfb or screenshot", func(v string) error {
		switch TileMode(v) {
		case TileRFB, TileScreenshot:
			c.TileMode = TileMode(v)
			return nil
		}
		return fmt.Errorf("must be %q or %q", TileRFB, TileScreenshot)
	})
	fs.Func("host-access", "host introspection: local, none or fake", func(v string) error {
		switch HostAccessMode(v) {
		case HostLocal, HostNone, HostFake:
			c.HostAccess = HostAccessMode(v)
			return nil
		}
		return fmt.Errorf("must be %q, %q or %q", HostLocal, HostNone, HostFake)
	})

	fs.BoolVar(&c.Verbose, "verbose", c.Verbose, "log at debug level")
}

// Validate reports configuration that cannot work.
func (c Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("listen address is required"))
	}
	if c.InventoryDir == "" {
		errs = append(errs, errors.New("inventory directory is required"))
	}
	if c.IdentityHeader == "" {
		errs = append(errs, errors.New("identity header name is required"))
	}
	if c.DefaultIdentity == "" {
		errs = append(errs, errors.New("default identity is required, since a lease holder must be named"))
	}
	if c.LeaseIdle <= 0 {
		errs = append(errs, errors.New("lease idle timeout must be positive"))
	}
	if c.LeaseWarn >= c.LeaseIdle {
		errs = append(errs, fmt.Errorf("lease warning (%s) must be shorter than the idle timeout (%s)",
			c.LeaseWarn, c.LeaseIdle))
	}
	if c.RingBytes <= 0 {
		errs = append(errs, errors.New("scrollback must be positive"))
	}
	if c.TranscriptDir == "" && c.TranscriptMaxFiles > 0 {
		// Not an error, just pointless; say so rather than silently
		// ignoring retention settings.
		errs = append(errs, errors.New("retention is configured but recordings-dir is empty, so nothing is captured"))
	}
	return errors.Join(errs...)
}
