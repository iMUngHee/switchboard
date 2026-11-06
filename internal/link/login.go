package link

import (
	"bytes"
	"context"
	"net"
	"os"
	"time"

	"github.com/iMUngHee/switchboard/internal/proc"
	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/transport"
)

// Resuming after a login. A link that ssh's credentials stopped waits for a
// person, but the person's fix is usually a login, `ssh HOST` once, and with
// a ControlMaster that login leaves a master listening on the host's
// ControlPath, which the link's own ssh then shares. A look for one connects
// to that local socket and nothing else. The ControlPath comes from `ssh -G`,
// which prints the configuration without connecting but does evaluate it: a
// Match exec in it runs, and canonicalization looks the host up. So it is
// asked once per stop, not at every look. ssh's own `-O check` is not used,
// since it connects to the far side when ControlMaster is yes or the master
// does not answer its hello.
const (
	LoginWatch  = 30 * time.Second // how often a link stopped over credentials looks for a master
	masterCheck = 5 * time.Second  // how long asking ssh -G, or one look, may take
)

// watchLogin resumes the link when a person's login has left an ssh master
// for the far side, until ctx ends. It resumes once per master: if the link
// stops over credentials again while the same one listens, the link's ssh is
// not sharing it, and only a new login or a resume helps. What it resumed for
// is kept in memory only, so a new process, after a restart or an upgrade,
// may resume once for a master an earlier one already tried.
func (l *Link) watchLogin(ctx context.Context) {
	if l.controlPath == nil {
		return
	}
	every := l.loginEvery
	if every <= 0 {
		every = LoginWatch
	}
	// A stop lasts from one resume to the next: each asks ssh -G again, in
	// case the configuration changed. Within a stop the answer holds, none
	// included: a ControlMaster added meanwhile takes a resume.
	path, asked, askedIn := "", false, uint64(0)
	for {
		l.mu.Lock()
		look, stop := l.stoppedOverLogin(), l.resumes
		l.mu.Unlock()
		if look {
			if !asked || askedIn != stop {
				actx, cancel := context.WithTimeout(ctx, masterCheck)
				path, asked, askedIn = l.controlPath(actx), true, stop
				cancel()
			}
			if m, ok := listening(ctx, path); ok {
				l.resumeOnLogin(m)
			}
		}
		t := time.NewTimer(l.jitter(every))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// resumeOnLogin resumes the link for the master whose socket is m, unless
// the link no longer waits on a login or m is the master it last resumed for.
// The look ran outside the lock, so the link may have changed meanwhile.
func (l *Link) resumeOnLogin(m os.FileInfo) {
	l.mu.Lock()
	if !l.stoppedOverLogin() {
		l.mu.Unlock()
		return
	}
	if l.resumedFor != nil && sameMaster(l.resumedFor, m) {
		l.mu.Unlock()
		l.opt.Log.Debug("ssh master unchanged since the last resume", "link", l.opt.Name)
		return
	}
	l.resumedFor = m
	l.resume()
	l.mu.Unlock()
	l.opt.Log.Info("link resumed", "link", l.opt.Name, "by", "ssh master")
	l.wakeAll()
}

// stoppedOverLogin says whether the link is stopped by ssh's credentials
// alone, which a login can fix: some direction ended in auth, and none in
// something a login cannot fix. The caller holds l.mu.
func (l *Link) stoppedOverLogin() bool {
	if !l.st.NeedsHuman {
		return false
	}
	auth := false
	for _, d := range l.dirs {
		switch round.Class(d.st.Class) {
		case round.Auth:
			auth = true
		case round.NeedsHuman:
			return false
		}
	}
	return auth
}

// sshControlPath is the ControlPath `ssh -G` names for s, or "" when it names
// none or does not answer.
func sshControlPath(ctx context.Context, s transport.SSHEndpoint) string {
	c := s.ConfigCommand()
	var out bytes.Buffer
	r := proc.Run(ctx, proc.Spec{Path: c.Path, Args: c.Args, Env: c.Env, Stdout: &out})
	if !r.Started || r.TimedOut || r.ExitCode != 0 {
		return ""
	}
	return transport.ControlPath(out.Bytes())
}

// listening returns the socket at path, as it was then, when a process
// listens on it: at a ControlPath, an ssh master.
func listening(ctx context.Context, path string) (os.FileInfo, bool) {
	if path == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, masterCheck)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, false
	}
	conn.Close()
	m, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	return m, true
}

// sameMaster says whether sockets a and b are one file: a new master makes a
// new one.
func sameMaster(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime())
}
