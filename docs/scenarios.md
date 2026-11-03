# Scenarios

## A laptop and a remote VM

The laptop can reach the VM over ssh; the VM cannot reach the laptop. So
switchboard runs on the laptop only, and keeps a `pager relay wait` open on the
VM that answers as soon as mail is waiting there; with a pager on the VM that
has no `relay wait`, it asks for the VM's mail every `poll_inbound` instead.
The VM needs only pager.

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

After the stores are paired (see the README), on the laptop:

```sh
switchboard check
switchboard service install
```

- If ssh needs a jump host or a hardware key, put that in `~/.ssh/config` under
  the host's alias. A ControlMaster there makes every round cheap.
- When the laptop sleeps, the shared connection dies. With a key in an agent,
  ssh connects again by itself. With one that needs a touch or a passphrase,
  or a jump host that asks for one, batch-mode ssh fails until a person runs
  `ssh vm.example.com` once. switchboard waits ten minutes after waking before
  it says the far side is offline, then shows the link's hint, and from then on
  tries it every 30 minutes, and at once whenever the laptop wakes. If ssh says
  the key or host key is the problem, it says so at once and stops trying, since
  every try would knock on the far side's login again: run `ssh vm.example.com`
  once, then `switchboard resume vm`. Nothing is lost meanwhile: mail waits on
  its sender until a round carries it.

## A dual-boot desktop and a Raspberry Pi

The desktop boots Windows or Linux; each has its own pager store, so the Pi
sees them as two peers, say `win` and `linux`, and each OS is a peer of the Pi.
switchboard runs on whichever OS is up, as a service installed on both; the Pi
runs only pager.

On Windows (`%AppData%\switchboard\config.toml`):

```toml
version = 1

[pager]
path = 'C:\Users\me\bin\pager.exe'   # a TOML literal string: no escapes

[link.pi]
remote_peer = "win"

[link.pi.ssh]
host = "pi.example.com"
pager = "/usr/local/bin/pager"
```

The Linux side is the same with `remote_peer = "linux"` and its own pager path.

- The Pi's sessions address the OS that is up: `kipa@win` or `kipa@linux`. Mail
  for the one that is down waits on its sender until it boots.
- Windows OpenSSH cannot share connections, so every round is a fresh
  handshake. An idle link with `relay wait` on the Pi makes about two every five
  minutes; without it, one every `poll_inbound`, which you can raise if that is
  too often.
- On Windows a link that needs you is written to the log, unless `[notify]`
  names a program to run.

## Limits

- **One hop.** pager does not forward mail it imported to a third store, so
  every pair of stores that exchanges mail must be peers, each with its own
  link. The two operating systems of a dual-boot desktop can never mail each
  other through the Pi.
- **No push.** The far side cannot start anything. Its mail arrives about a
  second after it is waiting while switchboard holds a `relay wait` open there,
  and within `poll_inbound` when the far pager has no `relay wait`.
- **The far shell.** The ssh link runs pager through the far side's login shell,
  which must be a POSIX one and print nothing to stdout before pager does. A
  banner there is caught by `switchboard check`.
- **One switchboard per link.** A second `switchboard run`, even with another
  configuration, waits for the first to let go of the link; `switchboard once`
  asks the one holding it to run instead.
