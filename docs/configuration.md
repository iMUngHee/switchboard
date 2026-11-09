# Configuration

switchboard reads one TOML file. It is strict: a key it does not know, a value
of the wrong type or a missing required key is an error naming the file and the
key, never something it skips, so a misspelt interval cannot quietly run on its
default. Every command says which file it read.

## Where it is

The first of these that is set wins:

1. `--config FILE` on the command line;
2. the `SWITCHBOARD_CONFIG` environment variable;
3. the default: `$XDG_CONFIG_HOME/switchboard/config.toml`, or
   `~/.config/switchboard/config.toml`, on macOS and Linux;
   `%AppData%\switchboard\config.toml` on Windows.

`switchboard service install` writes the absolute path of the file it read into
the service, so the service reads the same one.

## Every key

```toml
version = 1                          # required; this release reads version 1

[pager]
path = "/usr/local/bin/pager"        # required; absolute
db = "/home/me/.pager/msg.db"        # optional; absolute

[link.vm]                            # the table key is this store's peer name for the far store
remote_peer = "laptop"               # required; the far store's peer name for this store
poll_outbound = "5s"                 # optional; default 5s, at least 1s
poll_inbound = "30s"                 # optional; default 30s, at least 5s
hint = "run `ssh vm.example.com` once"   # optional; shown when the link needs you

[link.vm.ssh]                        # a link has exactly one of ssh and exec
host = "vm.example.com"              # required
pager = "/home/me/.local/bin/pager"  # required; absolute on the far side
options = ["-o", "IdentitiesOnly=yes"]   # optional; given to ssh before its own

[link.box]
remote_peer = "laptop"

[link.box.exec]
argv = ["/usr/bin/docker", "exec", "-i", "box", "/usr/local/bin/pager"]   # required
env = { DOCKER_CONTEXT = "default" }   # optional; set for argv's program

[notify]
command = ["/usr/local/bin/my-notifier", "{link}", "{state}", "{hint}"]   # optional
```

### version

The schema version, `1`. A file for a newer release says so, rather than
failing on keys this one does not know.

### [pager]

- `path` is the pager this side runs. Use the path the hooks use, the stable one
  a package manager links to, not a versioned directory behind it: a different
  pager may refuse the store, or migrate it under the hooks.
- `db` is the store, when it is not pager's default. switchboard always sets or
  clears `PAGER_DB` itself, so an inherited one never decides.

### [link.NAME]

`NAME` is the peer name this store gave the far store with `pager peer add`;
`remote_peer` is the name the far store gave this one. Peer names are 1 to 32
characters of `a-z`, `0-9` and `-`, starting with a letter or digit, written in
lower case: pager folds upper case, and a configuration naming `VM` would then
name a peer other than the one pager lists.

- `poll_outbound` is how often this store's pending mail is checked;
  `poll_inbound` how often the far store is asked for mail. A full batch is
  carried at once, without waiting for the next poll.
- Both are fallbacks when the pager mail comes from has `relay wait` (its
  `pager help` lists it): a direction with nothing left to carry asks that
  pager to say when mail is waiting, and runs its next round about a second
  after it is, or after five minutes at the latest. The intervals apply after a
  failure or a deferral, and against a pager without `relay wait`, which is
  asked again an hour later. While switchboard runs, `switchboard status` shows
  which a direction is doing: `waiting since` a time, or
  `polling: no relay wait there, asks again` at a time (in `--json`,
  waiting_since and no_wait_until).
- `hint` is what to do when the link needs you. For an ssh link it defaults to
  running `ssh HOST` once, which is what a laptop needs after a night's sleep.
  When that login leaves a ControlMaster up, for the host or for a jump host
  on its ProxyJump, a link stopped over ssh's credentials resumes by itself
  (see the laptop in [scenarios.md](scenarios.md)).

### [link.NAME.ssh]

The far side is reached with ssh in batch mode, `ssh OPTIONS -T -o BatchMode=yes
-o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -- HOST
'PAGER relay …'`. On Windows it adds `-o ControlMaster=no -o ControlPath=none`,
since Windows OpenSSH has no connection sharing.

- `host` is a host name, an ssh_config alias, `user@host` or a bracketed IPv6
  address. Only letters, digits, `.`, `_` and `-` are allowed in each part, each
  starting with a letter or digit, so nothing in it reaches a shell through a
  ProxyCommand.
- `pager` is the far side's pager, absolute, of `A-Z a-z 0-9 / . _ ~ + -` only,
  since it goes unquoted into the command the far shell runs. That shell must be
  a POSIX one; for anything else, use `exec` with your own ssh command.
- `options` come before switchboard's own, so they win: ssh keeps the first
  value of an option.

### [link.NAME.exec]

The far side is reached by running `argv` with pager's arguments after it, with
no shell: `docker exec`, `wsl.exe`, a different ssh, anything that runs pager
there. `env` is set for that program.

### [notify]

`command` runs for every notification, with `{link}`, `{state}` and `{hint}`
replaced inside its words; it gets nothing a relay stage printed. Without it,
macOS shows a notification, Linux uses `notify-send` when it is installed, and
Windows writes to the log. The states are `auth`, `offline`, `degraded`,
`needs-human`, and `ok` when the link works again. After `auth` the link stops
until `switchboard resume`, or until a login leaves a ControlMaster up; after
`offline` it is tried every 30 minutes, and at once when the machine wakes.

## State and logs

switchboard keeps each link's state, its lock and control requests in:

- Linux: `$XDG_STATE_HOME/switchboard`, or `~/.local/state/switchboard`;
- macOS: `~/Library/Application Support/switchboard`;
- Windows: `%LocalAppData%\switchboard`.

A link's files are named after this store's id and the link's name, so two
configurations over different stores never share one, and a lock keeps a second
switchboard off a link that is already being driven.

The service started by `switchboard service install` logs to `switchboard.log`
in that directory on macOS and Windows (moved aside at 10 MiB, three files kept)
and to the journal on Linux. The log names links, stages, exit codes and fixed
diagnostic codes; it never holds what a stage wrote to stderr, which can name
the people mail is for.
