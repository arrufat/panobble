// Package notify is a minimal org.freedesktop.Notifications client: Notify,
// CloseNotification and the ActionInvoked / NotificationClosed signals, on a
// private session-bus connection.
//
// This package has no pano-scrobbler counterpart: upstream renders its
// now-playing state in a JVM tray menu on desktop and in Android
// notifications. The behaviour here follows the freedesktop Desktop
// Notifications Specification instead.
package notify

import (
	"context"
	"fmt"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	busName = "org.freedesktop.Notifications"
	objPath = dbus.ObjectPath("/org/freedesktop/Notifications")
	iface   = "org.freedesktop.Notifications"
)

// Close reasons carried by NotificationClosed.
const (
	ReasonExpired   uint32 = 1
	ReasonDismissed uint32 = 2 // dismissed by the user
	ReasonClosed    uint32 = 3 // CloseNotification call
	ReasonUndefined uint32 = 4
)

// Notification is one Notify call.
type Notification struct {
	ReplacesID uint32 // 0 = new notification
	AppIcon    string // themed icon name or absolute path; "" for none
	Summary    string // plain text
	Body       string // markup where the server supports it; see EscapeMarkup
	Actions    []string
	Hints      map[string]dbus.Variant
	Timeout    int32 // milliseconds; -1 = server default, 0 = never expire
}

// Client talks to the notification server. Callbacks run on the signal
// goroutine (Run) and must not block.
type Client struct {
	conn *dbus.Conn
	obj  dbus.BusObject
	sigs chan *dbus.Signal

	OnAction func(id uint32, action string)
	OnClosed func(id uint32, reason uint32)
}

// Connect opens a private session-bus connection and subscribes to the
// server's signals. Signals are addressed to the connection that sent the
// notification, which is why this connection is not shared.
func Connect() (*Client, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connecting to session bus: %w", err)
	}
	// Servers that address their signals to the client (GNOME) reach us
	// without any rule; the rules cover servers that broadcast. Restricting
	// the sender to the name owner matters on GNOME 51, where gnome-shell
	// broadcasts a second copy of every signal the (separate) notification
	// server already sent us directly.
	for _, member := range []string{"ActionInvoked", "NotificationClosed"} {
		if err := conn.AddMatchSignal(
			dbus.WithMatchSender(busName),
			dbus.WithMatchObjectPath(objPath),
			dbus.WithMatchInterface(iface),
			dbus.WithMatchMember(member),
		); err != nil {
			conn.Close()
			return nil, err
		}
	}
	c := &Client{
		conn: conn,
		obj:  conn.Object(busName, objPath),
		sigs: make(chan *dbus.Signal, 16),
	}
	conn.Signal(c.sigs)
	return c, nil
}

// Run dispatches signals to the callbacks until the connection closes.
func (c *Client) Run() {
	for sig := range c.sigs {
		switch sig.Name {
		case iface + ".ActionInvoked":
			if len(sig.Body) != 2 || c.OnAction == nil {
				continue
			}
			id, ok1 := sig.Body[0].(uint32)
			action, ok2 := sig.Body[1].(string)
			if ok1 && ok2 {
				c.OnAction(id, action)
			}
		case iface + ".NotificationClosed":
			if len(sig.Body) != 2 || c.OnClosed == nil {
				continue
			}
			id, ok1 := sig.Body[0].(uint32)
			reason, ok2 := sig.Body[1].(uint32)
			if ok1 && ok2 {
				c.OnClosed(id, reason)
			}
		}
	}
}

// Notify shows or updates a notification and returns its id.
func (c *Client) Notify(ctx context.Context, n Notification) (uint32, error) {
	actions := n.Actions
	if actions == nil {
		actions = []string{}
	}
	hints := n.Hints
	if hints == nil {
		hints = map[string]dbus.Variant{}
	}
	var id uint32
	err := c.obj.CallWithContext(ctx, iface+".Notify", 0,
		"panobble", n.ReplacesID, n.AppIcon, n.Summary, n.Body, actions, hints, n.Timeout,
	).Store(&id)
	return id, err
}

// CloseNotification removes a notification.
func (c *Client) CloseNotification(ctx context.Context, id uint32) error {
	return c.obj.CallWithContext(ctx, iface+".CloseNotification", 0, id).Err
}

// Capabilities returns the server's advertised capabilities.
func (c *Client) Capabilities(ctx context.Context) ([]string, error) {
	var caps []string
	err := c.obj.CallWithContext(ctx, iface+".GetCapabilities", 0).Store(&caps)
	return caps, err
}

// Close closes the connection; Run returns afterwards.
func (c *Client) Close() error { return c.conn.Close() }

var markupEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// EscapeMarkup escapes text for a markup-capable body.
func EscapeMarkup(s string) string { return markupEscaper.Replace(s) }
