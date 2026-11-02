// Package notify tells a person when a link needs them, and when it works
// again.
//
// What it says is built only from the link's name, its health and the hint
// in the configuration: never from anything a stage printed, which can name
// recipients or quote what they were sent.
//
// Notifications go through one worker in the order they were made, so a
// slow notifier never holds up a round, and the last one a person sees is
// the latest.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/iMUngHee/switchboard/internal/link"
	"github.com/iMUngHee/switchboard/internal/proc"
)

const (
	// Timeout bounds one notifier command.
	Timeout = 10 * time.Second
	// queueSize is how many notifications may wait; past it the oldest
	// waiting ones are dropped and logged, since a newer one supersedes them.
	queueSize = 16
	// closeWait bounds how long Close waits for what is queued, so a hanging
	// notifier never holds up a daemon that is stopping.
	closeWait = 15 * time.Second
)

// Message is one notification: who, what, and what to do.
type Message struct {
	Link   string
	Health link.Health
	Hint   string
}

// Title and Body are what a desktop notification shows.
func (m Message) Title() string { return "switchboard: " + m.Link }

func (m Message) Body() string {
	var s string
	switch m.Health {
	case link.NeedsHuman:
		s = "stopped; it needs you, then `switchboard resume " + m.Link + "`."
	case link.Auth:
		s = "ssh needs a key loaded or a host key accepted."
	case link.Offline:
		s = "the far side has not answered for 10 minutes."
	case link.Degraded:
		s = "keeps failing the same way; still retrying."
	case link.Healthy:
		return "works again."
	default:
		s = string(m.Health) + "."
	}
	if m.Hint != "" {
		s += " " + m.Hint
	}
	return s
}

// Sender delivers one message.
type Sender func(ctx context.Context, m Message) error

// Notifier queues messages for a Sender. It implements link.Notifier.
type Notifier struct {
	send    Sender
	log     *slog.Logger
	timeout time.Duration
	queue   chan Message
	done    chan struct{}
}

// New returns a Notifier for the configured command, or, with none, for this
// system's own notifications. Close it to stop its worker.
func New(command []string, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	send := Default(log)
	if len(command) > 0 {
		send = Command(command)
	}
	return start(send, log)
}

func start(send Sender, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	n := &Notifier{send: send, log: log, timeout: Timeout, queue: make(chan Message, queueSize), done: make(chan struct{})}
	go n.work()
	return n
}

// Notify queues a message. It never blocks.
func (n *Notifier) Notify(name string, h link.Health, hint string) {
	m := Message{Link: name, Health: h, Hint: hint}
	for {
		select {
		case n.queue <- m:
			return
		default:
		}
		select {
		case old := <-n.queue:
			n.log.Warn("notification dropped for a newer one", "link", old.Link, "health", string(old.Health))
		default:
		}
	}
}

// Close sends what is queued, for up to closeWait, then stops. Call it after
// the last Notify.
func (n *Notifier) Close() {
	close(n.queue)
	select {
	case <-n.done:
	case <-time.After(closeWait):
		n.log.Warn("notifications still queued at exit were not sent")
	}
}

func (n *Notifier) work() {
	defer close(n.done)
	for m := range n.queue {
		ctx, cancel := context.WithTimeout(context.Background(), n.timeout)
		err := n.send(ctx, m)
		cancel()
		if err != nil {
			n.log.Warn("notification failed", "link", m.Link, "health", string(m.Health), "err", err)
		} else {
			n.log.Info("notified", "link", m.Link, "health", string(m.Health))
		}
	}
}

// Command runs argv for each message, with {link}, {state} and {hint}
// replaced inside each word. No shell is involved.
func Command(argv []string) Sender {
	return func(ctx context.Context, m Message) error {
		r := strings.NewReplacer("{link}", m.Link, "{state}", string(m.Health), "{hint}", m.Hint)
		words := make([]string, len(argv))
		for i, a := range argv {
			words[i] = r.Replace(a)
		}
		return run(ctx, words[0], words[1:]...)
	}
}

// Default is this system's own notification: a macOS notification through
// osascript, notify-send on Linux when it is installed, and on Windows, and
// anywhere else, only the log.
func Default(log *slog.Logger) Sender {
	notifySend, _ := exec.LookPath("notify-send")
	if argv := systemArgv(runtime.GOOS, notifySend, Message{}); argv != nil {
		return func(ctx context.Context, m Message) error {
			argv := systemArgv(runtime.GOOS, notifySend, m)
			return run(ctx, argv[0], argv[1:]...)
		}
	}
	return func(_ context.Context, m Message) error {
		log.Warn("a link needs attention", "link", m.Link, "health", string(m.Health), "message", m.Body())
		return nil
	}
}

// systemArgv is the command that shows m on goos, or nil where there is
// none. The text is always an argument of its own: osascript gets it through
// argv, never inside its script, and notify-send gets it after "--".
func systemArgv(goos, notifySend string, m Message) []string {
	switch {
	case goos == "darwin":
		return []string{"/usr/bin/osascript",
			"-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run",
			m.Title(), m.Body()}
	case goos == "linux" && notifySend != "":
		return []string{notifySend, "--app-name=switchboard", "--", m.Title(), m.Body()}
	}
	return nil
}

func run(ctx context.Context, path string, args ...string) error {
	r := proc.Run(ctx, proc.Spec{Path: path, Args: args})
	switch {
	case r.Err != nil:
		return r.Err
	case r.TimedOut:
		return fmt.Errorf("%s did not finish in time", path)
	case r.ExitCode != 0:
		return fmt.Errorf("%s exited %d", path, r.ExitCode)
	}
	return nil
}
