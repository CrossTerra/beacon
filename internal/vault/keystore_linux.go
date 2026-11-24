package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/godbus/dbus/v5"
)

const service = "org.freedesktop.secrets"
const secretAPI = "org.freedesktop.Secret."

type itemSecret struct {
	Session dbus.ObjectPath
	Params  []byte
	Value   []byte
	Type    string
}

func excludeFromBackup(string) error { return nil }

func sessionBus() (*dbus.Conn, error) {
	addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		dir := os.Getenv("XDG_RUNTIME_DIR")
		if !filepath.IsAbs(dir) {
			return nil, ErrKeyMissing
		}
		addr = "unix:path=" + dbus.EscapeBusAddressValue(filepath.Join(dir, "bus"))
	}
	return dbus.Connect(addr) // don't autolaunch a session bus from a background job
}

func openSession(ctx context.Context, c *dbus.Conn) (dbus.ObjectPath, error) {
	var ignored dbus.Variant
	var path dbus.ObjectPath
	err := c.Object(service, "/org/freedesktop/secrets").CallWithContext(ctx, secretAPI+"Service.OpenSession", 0,
		"plain", dbus.MakeVariant("")).Store(&ignored, &path)
	return path, err
}

func keyAttributes(id string) map[string]string {
	return map[string]string{"application": "dev.beacon", "unattended-vault": id}
}

func saveKeyToSession(ctx context.Context, id string, key []byte) (bool, error) {
	c, err := sessionBus()
	if err != nil {
		return false, nil // no available session bus at setup
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	session, err := openSession(ctx, c)
	if err != nil {
		var busErr dbus.Error
		if errors.As(err, &busErr) && (busErr.Name == "org.freedesktop.DBus.Error.ServiceUnknown" ||
			busErr.Name == "org.freedesktop.DBus.Error.NameHasNoOwner") {
			return false, nil
		}
		return false, ErrLocked
	}
	var collection dbus.ObjectPath
	err = c.Object(service, "/org/freedesktop/secrets").CallWithContext(ctx, secretAPI+"Service.ReadAlias", 0, "default").Store(&collection)
	if err != nil || collection == "/" || !collection.IsValid() {
		return false, ErrLocked
	}
	var locked dbus.Variant
	err = c.Object(service, collection).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
		secretAPI+"Collection", "Locked").Store(&locked)
	if err != nil || locked.Value() != false {
		return false, ErrLocked
	}
	props := map[string]dbus.Variant{
		secretAPI + "Item.Label":      dbus.MakeVariant("Beacon unattended vault key"),
		secretAPI + "Item.Attributes": dbus.MakeVariant(keyAttributes(id)),
	}
	var item, prompt dbus.ObjectPath
	err = c.Object(service, collection).CallWithContext(ctx, secretAPI+"Collection.CreateItem", 0, props,
		itemSecret{session, []byte{}, key, "application/xml"}, false).Store(&item, &prompt)
	if err != nil || prompt != "/" || !item.IsValid() || item == "/" {
		return false, ErrLocked // do not launch an unlock dialog or silently switch to a file
	}
	return true, nil
}

func loadSessionKey(id string) ([]byte, error) {
	c, err := sessionBus()
	if err != nil {
		return nil, ErrKeyMissing
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := c.Object(service, "/org/freedesktop/secrets")
	var ready, locked []dbus.ObjectPath
	if err := s.CallWithContext(ctx, secretAPI+"Service.SearchItems", 0, keyAttributes(id)).Store(&ready, &locked); err != nil {
		return nil, ErrKeyMissing
	}
	if len(ready)+len(locked) > 1 {
		return nil, ErrAmbiguous
	}
	if len(locked) != 0 {
		return nil, ErrLocked
	}
	if len(ready) == 0 {
		return nil, ErrKeyMissing
	}
	session, err := openSession(ctx, c)
	if err != nil {
		return nil, ErrLocked
	}
	var key itemSecret
	err = c.Object(service, ready[0]).CallWithContext(ctx, secretAPI+"Item.GetSecret", 0, session).Store(&key)
	if err != nil || key.Session != session || len(key.Value) == 0 || len(key.Value) > 4096 {
		clear(key.Value)
		return nil, ErrUnreadable
	}
	return key.Value, nil
}
