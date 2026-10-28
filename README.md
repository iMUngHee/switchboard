# switchboard

switchboard carries [pager](https://github.com/iMUngHee/pager) mail between pager
stores on different machines. pager keeps one store per machine and never opens
a network connection; its relay contract (`relay out | relay in | relay ack`)
moves mail between two stores over anything that carries lines. switchboard is
that transport: it decides when to run a round, runs it over ssh or any command
that reaches the other side, retries what can be retried, and tells you when
only you can fix what went wrong.

It runs on macOS, Linux and Windows, as the user whose pager it serves. The
other side needs only pager.

**Status: in development.** Nothing here is ready to use yet.

## License

MIT — see [LICENSE](LICENSE).
