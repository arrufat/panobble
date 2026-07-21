package mpris

import (
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	mprisPath       = "/org/mpris/MediaPlayer2"
	playerInterface = "org.mpris.MediaPlayer2.Player"
	rootInterface   = "org.mpris.MediaPlayer2"
	propsInterface  = "org.freedesktop.DBus.Properties"
)

// Player is a currently-present MPRIS player.
type Player struct {
	BusName  string // org.mpris.MediaPlayer2.spotify
	Owner    string // unique name :1.42
	Identity string // human-readable, e.g. "Spotify"
}

// Event is one normalized change from a player, consumed by the tracker.
type Event struct {
	BusName string
	AppID   string // normalized

	// Exactly one of the following is set.
	Metadata *Metadata       // metadata changed
	Status   *PlaybackStatus // playback status changed
	SeekedTo *time.Duration  // seeked
	Removed  bool            // player disappeared

	// Position at the time of a status change, -1 if unavailable.
	Position time.Duration
	// Only fetched for Status events (feeds the Spotify ad heuristic).
	CanGoNext bool
}

// Watcher owns the D-Bus connection.
type Watcher struct {
	conn   *dbus.Conn
	events chan Event
	// unique name -> bus name, to attribute PropertiesChanged senders
	owners map[string]string
}

func NewWatcher() (*Watcher, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connecting to session bus: %w", err)
	}
	return &Watcher{
		conn:   conn,
		events: make(chan Event, 32),
		owners: make(map[string]string),
	}, nil
}

func (w *Watcher) Close() error { return w.conn.Close() }

// ListPlayers enumerates current MPRIS players.
func (w *Watcher) ListPlayers() ([]Player, error) {
	var names []string
	err := w.conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&names)
	if err != nil {
		return nil, err
	}

	var players []Player
	for _, name := range names {
		if !strings.HasPrefix(name, busPrefix) {
			continue
		}
		p := Player{BusName: name}
		w.conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, name).Store(&p.Owner)
		obj := w.conn.Object(name, mprisPath)
		if v, err := obj.GetProperty(rootInterface + ".Identity"); err == nil {
			if s, ok := v.Value().(string); ok {
				p.Identity = s
			}
		}
		players = append(players, p)
	}
	return players, nil
}

// Snapshot fetches the current metadata and playback status of a player.
func (w *Watcher) Snapshot(busName string) (Metadata, PlaybackStatus, error) {
	obj := w.conn.Object(busName, mprisPath)
	var props map[string]dbus.Variant
	err := obj.Call(propsInterface+".GetAll", 0, playerInterface).Store(&props)
	if err != nil {
		return Metadata{}, StatusUnknown, err
	}
	md := Metadata{}
	if v, ok := props["Metadata"]; ok {
		if m, ok := v.Value().(map[string]dbus.Variant); ok {
			md = metadataFromMap(m)
		}
	}
	status := StatusUnknown
	if v, ok := props["PlaybackStatus"]; ok {
		if s, ok := v.Value().(string); ok {
			status = ParsePlaybackStatus(s)
		}
	}
	return md, status, nil
}

// Position fetches the current playback position, or -1 if unavailable.
func (w *Watcher) Position(busName string) time.Duration {
	obj := w.conn.Object(busName, mprisPath)
	v, err := obj.GetProperty(playerInterface + ".Position")
	if err != nil {
		return -1
	}
	if us := asInt64(v.Value()); us >= 0 {
		return time.Duration(us) * time.Microsecond
	}
	return -1
}

// CanGoNext fetches the CanGoNext property (used by the Spotify ad heuristic).
func (w *Watcher) CanGoNext(busName string) bool {
	obj := w.conn.Object(busName, mprisPath)
	v, err := obj.GetProperty(playerInterface + ".CanGoNext")
	if err != nil {
		return true
	}
	b, ok := v.Value().(bool)
	return !ok || b
}

// Events returns the event channel fed by Run.
func (w *Watcher) Events() <-chan Event { return w.events }

// Run subscribes to signals and translates them into Events until the
// connection closes. It must only be called once.
func (w *Watcher) Run() error {
	if err := w.conn.AddMatchSignal(
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg0Namespace("org.mpris.MediaPlayer2"),
	); err != nil {
		return err
	}
	if err := w.conn.AddMatchSignal(
		dbus.WithMatchObjectPath(mprisPath),
		dbus.WithMatchInterface(propsInterface),
		dbus.WithMatchMember("PropertiesChanged"),
	); err != nil {
		return err
	}
	if err := w.conn.AddMatchSignal(
		dbus.WithMatchObjectPath(mprisPath),
		dbus.WithMatchInterface(playerInterface),
		dbus.WithMatchMember("Seeked"),
	); err != nil {
		return err
	}

	// Seed owners map and emit initial snapshots.
	players, err := w.ListPlayers()
	if err != nil {
		return err
	}
	for _, p := range players {
		if p.Owner != "" {
			w.owners[p.Owner] = p.BusName
		}
		if md, status, err := w.Snapshot(p.BusName); err == nil {
			appID := NormalizeAppID(p.BusName)
			w.events <- Event{BusName: p.BusName, AppID: appID, Metadata: &md,
				Position: -1}
			w.events <- Event{BusName: p.BusName, AppID: appID, Status: &status,
				Position: w.Position(p.BusName), CanGoNext: w.CanGoNext(p.BusName)}
		}
	}

	sigs := make(chan *dbus.Signal, 64)
	w.conn.Signal(sigs)
	for sig := range sigs {
		w.handleSignal(sig)
	}
	close(w.events)
	return nil
}

func (w *Watcher) handleSignal(sig *dbus.Signal) {
	switch sig.Name {
	case "org.freedesktop.DBus.NameOwnerChanged":
		if len(sig.Body) != 3 {
			return
		}
		name, _ := sig.Body[0].(string)
		oldOwner, _ := sig.Body[1].(string)
		newOwner, _ := sig.Body[2].(string)
		if !strings.HasPrefix(name, busPrefix) {
			return
		}
		if oldOwner != "" {
			delete(w.owners, oldOwner)
		}
		if newOwner == "" {
			w.events <- Event{BusName: name, AppID: NormalizeAppID(name), Removed: true, Position: -1}
		} else {
			w.owners[newOwner] = name
		}

	case propsInterface + ".PropertiesChanged":
		busName, ok := w.owners[sig.Sender]
		if !ok {
			return
		}
		if len(sig.Body) < 2 {
			return
		}
		iface, _ := sig.Body[0].(string)
		if iface != playerInterface {
			return
		}
		changed, _ := sig.Body[1].(map[string]dbus.Variant)
		appID := NormalizeAppID(busName)

		// Metadata first, then playback status; the tracker relies on this
		// ordering.
		if v, ok := changed["Metadata"]; ok {
			if m, ok := v.Value().(map[string]dbus.Variant); ok {
				md := metadataFromMap(m)
				w.events <- Event{BusName: busName, AppID: appID, Metadata: &md,
					Position: -1}
			}
		}
		if v, ok := changed["PlaybackStatus"]; ok {
			if s, ok := v.Value().(string); ok {
				status := ParsePlaybackStatus(s)
				w.events <- Event{BusName: busName, AppID: appID, Status: &status,
					Position: w.Position(busName), CanGoNext: w.CanGoNext(busName)}
			}
		}

	case playerInterface + ".Seeked":
		busName, ok := w.owners[sig.Sender]
		if !ok || len(sig.Body) != 1 {
			return
		}
		if us := asInt64(sig.Body[0]); us >= 0 {
			pos := time.Duration(us) * time.Microsecond
			w.events <- Event{BusName: busName, AppID: NormalizeAppID(busName),
				SeekedTo: &pos, Position: pos}
		}
	}
}
