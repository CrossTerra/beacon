package notice

import (
	"context"
	"os"
	"path/filepath"

	"github.com/godbus/dbus/v5"
)

func Send(ctx context.Context, message string) {
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		dir := os.Getenv("XDG_RUNTIME_DIR")
		if !filepath.IsAbs(dir) {
			return
		}
		addr = "unix:path=" + dbus.EscapeBusAddressValue(filepath.Join(dir, "bus"))
	}
	c, err := dbus.Connect(addr)
	if err != nil {
		return
	}
	defer c.Close()
	const api = "org.freedesktop.Notifications"
	_ = c.Object(api, "/org/freedesktop/Notifications").CallWithContext(ctx, api+".Notify", 0,
		"Beacon", uint32(0), "", "Beacon", message, []string{}, map[string]dbus.Variant{}, int32(-1)).Err
}
