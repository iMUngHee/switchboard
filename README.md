# switchboard

switchboard carries [pager](https://github.com/iMUngHee/pager) mail between pager
stores on different machines. pager keeps one store per machine and never opens
a network connection; its relay contract (`relay out | relay in | relay ack`)
moves mail between two stores over anything that carries lines. switchboard is
that transport: it decides when to run a round, runs it over ssh or any command
that reaches the other side, retries what can be retried, and tells you when
only you can fix what went wrong.

It runs on macOS, Linux and Windows, as the user whose pager it serves. It runs
only on the machine that can reach the other one; the other side needs only
pager.

## Install

```sh
go install github.com/iMUngHee/switchboard/cmd/switchboard@latest
```

On Windows, also build the console-less copy the service runs, so no console
window opens at logon, and keep it beside `switchboard.exe`:

```sh
go build -ldflags=-H=windowsgui -o switchboardw.exe github.com/iMUngHee/switchboard/cmd/switchboard
```

## Pair the two stores

Each store must know the other by its id, so a transport that lands on the
wrong database fails instead of delivering there. Here the laptop calls the
other store `vm`, and the other store calls the laptop `laptop`:

```sh
laptop$ pager relay id                     # prints the laptop's store id
vm$     pager relay id                     # prints the vm's store id
laptop$ pager peer add vm --store <the vm's id>
vm$     pager peer add laptop --store <the laptop's id>
```

Or, once the link is configured (below), let switchboard do all four over it:
`switchboard enroll` adds whichever side is missing, can be run again if it
stops halfway, and never changes a name already enrolled as another store.

## Configure

`~/.config/switchboard/config.toml` (on Windows `%AppData%\switchboard\config.toml`):

```toml
version = 1

[pager]
path = "/usr/local/bin/pager"

[link.vm]
remote_peer = "laptop"

[link.vm.ssh]
host = "vm.example.com"
pager = "/home/me/.local/bin/pager"
```

Every key is described in [docs/configuration.md](docs/configuration.md).

## Run

```sh
switchboard enroll           # enroll both stores with each other, if not yet
switchboard check            # both stores enrolled each other, and ssh works
switchboard once             # one round each way, now
switchboard service install  # run at logon, as you
switchboard status           # what each link is doing
```

`switchboard service install` registers a launchd agent on macOS, a systemd
user unit on Linux or a Task Scheduler task on Windows, all for you alone, and
starts it; `switchboard service install --print` shows what it would write.
On a Linux machine you do not stay logged in to, run `loginctl enable-linger`
so the unit runs without you; install says so when it is needed. It runs the
switchboard you started it with, or `--exe FILE`. `switchboard service status`
says whether it is installed (and, on macOS and Linux, running) and whether a
switchboard holds each link, and `switchboard service uninstall` stops and
removes it.

`switchboard run` is what the service runs. When a link needs you, switchboard
tells you once: a desktop notification on macOS and on Linux with notify-send,
the log on Windows, or any program you name in `[notify]`. Fix what it says,
then `switchboard resume vm`. `switchboard once --debug` shows what each stage
wrote to stderr, which switchboard otherwise never shows or keeps.

Two machines in practice, a laptop with a remote VM and a dual-boot desktop
with a Raspberry Pi, and what switchboard cannot do, are in
[docs/scenarios.md](docs/scenarios.md).

## License

MIT — see [LICENSE](LICENSE).
