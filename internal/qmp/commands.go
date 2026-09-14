package qmp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// keyPattern constrains a key name. QEMU names a key with a qcode, such as
// "ctrl" or "f3", and the names come from a client, so a name that cannot be
// a qcode is refused here rather than passed on.
var keyPattern = regexp.MustCompile(`^[a-z0-9_]{1,24}$`)

// MaxKeys bounds one chord. QEMU presses every key of a call together and
// releases them together, and no chord needs more than this.
const MaxKeys = 8

// SendKey presses keys together and releases them, which is what makes one
// call one chord. holdMs is how long QEMU holds them down.
func (c *Client) SendKey(ctx context.Context, keys []string, holdMs int) error {
	if err := ValidateKeys(keys); err != nil {
		return err
	}
	qcodes := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		qcodes = append(qcodes, map[string]string{"type": "qcode", "data": k})
	}
	arguments := map[string]any{"keys": qcodes}
	if holdMs > 0 {
		arguments["hold-time"] = holdMs
	}
	_, err := c.Execute(ctx, "send-key", arguments)
	return err
}

// Screendump writes the current screen to path, as PNG.
//
// The format is always png. The same frame measures 10,790 bytes as PNG and
// 864,015 bytes as PPM, and QEMU defaults to PPM: the filename extension is
// not consulted.
//
// QEMU writes the file itself, as its own user and in its own filesystem
// namespace, so path must be absolute and its directory must be writable by
// QEMU. The command is synchronous, so the file is complete when the reply
// arrives.
func (c *Client) Screendump(ctx context.Context, path string) error {
	_, err := c.Execute(ctx, "screendump", map[string]any{
		"filename": path,
		"format":   "png",
	})
	return err
}

// ValidateKeys reports a chord labview will not send.
func ValidateKeys(keys []string) error {
	if len(keys) == 0 {
		return errors.New("name at least one key")
	}
	if len(keys) > MaxKeys {
		return fmt.Errorf("a chord holds %d keys at most", MaxKeys)
	}
	for _, k := range keys {
		if !keyPattern.MatchString(k) {
			return fmt.Errorf("key %q is not a QEMU key name", k)
		}
	}
	return nil
}
