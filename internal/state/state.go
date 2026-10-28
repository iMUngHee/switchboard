// Package state keeps what switchboard must remember about a link between
// runs: per direction, the last outcome and when to try again, and the result
// lines still waiting for `relay ack`.
//
// Everything is filed under the link's identity key, <local store id>.<peer>,
// so two configurations over different stores never share a file, and the OS
// lock on that key keeps a second process off a link that is already driven:
//
//	<state>/<key>.json             the state, replaced atomically
//	<state>/<key>.lock             the lock; never removed
//	<state>/control/<key>.<kind>   a request for whoever holds the lock
//
// One process writes a link's state at a time: the one holding its Lock. Save
// does not check that; callers take the lock first.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// schemaVersion is the state file format this release reads and writes.
const schemaVersion = 1

// Control request kinds.
const (
	KindResume = "resume"
	KindOnce   = "once"
)

// ErrLocked means another process, or another Lock in this one, holds the
// link.
var ErrLocked = errors.New("the link is held by another switchboard process")

// Key is a link's identity: this store's id and the peer name it gives the
// far store.
func Key(localStoreID, peer string) string {
	return localStoreID + "." + peer
}

// Direction is one direction of a link.
type Direction struct {
	Class               string    `json:"class,omitempty"`
	Code                string    `json:"code,omitempty"`
	LastSuccess         time.Time `json:"last_success,omitzero"` // last round with finals
	LastContact         time.Time `json:"last_contact,omitzero"` // last stage that reached the far side and exited as pager does
	NotBefore           time.Time `json:"not_before,omitzero"`
	ConsecutiveFailures int       `json:"consecutive_failures,omitempty"`
	// PendingAck holds correlated result lines awaiting `relay ack`,
	// verbatim, each without its LF. They are written here before ack runs,
	// so a restart acks them first.
	PendingAck []string `json:"pending_ack,omitempty"`
}

// Link is a link's state. The zero value is a link that has never run.
type Link struct {
	Outbound        Direction `json:"outbound"`
	Inbound         Direction `json:"inbound"`
	LastLocalStore  string    `json:"last_local_store,omitempty"`
	LastRemoteStore string    `json:"last_remote_store,omitempty"`
	NeedsHuman      bool      `json:"needs_human,omitempty"` // survives restarts
}

// Resume clears what holds a link back — its needs-human mark and each
// direction's backoff — so its next round runs at once. Everything else,
// pending results above all, stays.
func (l *Link) Resume() {
	l.NeedsHuman = false
	for _, d := range []*Direction{&l.Outbound, &l.Inbound} {
		d.NotBefore = time.Time{}
		d.ConsecutiveFailures = 0
	}
}

// file is the state file: Link with the schema version beside its fields.
type file struct {
	Version int `json:"version"`
	Link
}

// Load reads a link's state. A link with no state file yet has the zero
// value.
func Load(stateDir, key string) (Link, error) {
	if err := checkKey(key); err != nil {
		return Link{}, err
	}
	path := statePath(stateDir, key)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Link{}, nil
	}
	if err != nil {
		return Link{}, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return Link{}, fmt.Errorf("state %s: %w", path, err)
	}
	if f.Version != schemaVersion {
		return Link{}, fmt.Errorf("state %s: version %d is not one this release reads (%d)", path, f.Version, schemaVersion)
	}
	return f.Link, nil
}

// Save replaces a link's state and returns once the new state is on disk: it
// writes a temp file beside the state file, syncs it, and renames it over.
// The caller holds the link's Lock.
func Save(stateDir, key string, l Link) error {
	return save(stateDir, key, l, nil)
}

// save is Save with a hook between the synced temp file and the rename. A
// hook that fails ends save right there, as a crash would: the temp file
// stays and the old state is untouched.
func save(stateDir, key string, l Link, beforeRename func() error) error {
	if err := checkKey(key); err != nil {
		return err
	}
	data, err := json.MarshalIndent(file{Version: schemaVersion, Link: l}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, key+".json.tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if beforeRename != nil {
		if err := beforeRename(); err != nil {
			return err
		}
	}
	if err := replace(tmp.Name(), statePath(stateDir, key)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Lock is a link's identity lock, held until Close. The OS releases it if
// the process dies first.
type Lock struct{ f *os.File }

// TryLock takes a link's lock without waiting, or returns ErrLocked if it is
// held.
func TryLock(stateDir, key string) (*Lock, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, key+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Close releases the lock. The lock file stays: removing it would let two
// processes each lock a different file of the same name.
func (l *Lock) Close() error {
	uerr := unlockFile(l.f)
	if err := l.f.Close(); err != nil {
		return err
	}
	return uerr
}

// Request leaves a request of kind (KindResume or KindOnce) for whoever holds
// the link. Asking twice before it is taken is one request.
func Request(stateDir, key, kind string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if kind != KindResume && kind != KindOnce {
		return fmt.Errorf("state: unknown control request %q", kind)
	}
	dir := filepath.Join(stateDir, "control")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, key+"."+kind), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// Take removes and returns the link's pending requests, in a fixed order.
// Removing a request is what claims it, so two takers never both act on one.
// On an error it still returns what it took.
func Take(stateDir, key string) ([]string, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	var kinds []string
	for _, kind := range []string{KindResume, KindOnce} {
		err := os.Remove(filepath.Join(stateDir, "control", key+"."+kind))
		switch {
		case err == nil:
			kinds = append(kinds, kind)
		case errors.Is(err, fs.ErrNotExist):
		default:
			return kinds, err
		}
	}
	return kinds, nil
}

// Resume clears a link's needs-human mark and backoff (Link.Resume). With no
// process holding the link it does so itself, under the link's lock and on
// state re-read there. When the lock is held it leaves a resume request for
// the holder instead and reports queued.
func Resume(stateDir, key string) (queued bool, err error) {
	lock, err := TryLock(stateDir, key)
	if errors.Is(err, ErrLocked) {
		return true, Request(stateDir, key, KindResume)
	}
	if err != nil {
		return false, err
	}
	defer lock.Close()
	l, err := Load(stateDir, key)
	if err != nil {
		return false, err
	}
	l.Resume()
	return false, Save(stateDir, key, l)
}

func statePath(stateDir, key string) string {
	return filepath.Join(stateDir, key+".json")
}

// checkKey refuses a key that could name a file outside the state directory.
// Store ids are hex and peer names are [a-z0-9-], so a real key never trips
// it.
func checkKey(key string) error {
	if key == "" || key[0] == '.' {
		return fmt.Errorf("state: invalid key %q", key)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '-' || c == '_') {
			return fmt.Errorf("state: invalid key %q", key)
		}
	}
	return nil
}
