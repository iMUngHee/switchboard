// Package service installs switchboard as a per-user background service:
// a launchd agent on macOS, a systemd user unit on Linux, a Task Scheduler
// task on Windows. Each runs as the user whose pager store and hooks it
// serves; a system-wide service would run under another account and relay
// the wrong store.
package service
