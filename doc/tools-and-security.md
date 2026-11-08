# Tools and security

**Tools are enabled by default.** The model can run commands and modify files
in the current workspace without per-command approval. Use `-n` to disable all
tools. The sandbox does not isolate the network or all host data.

## Sandbox boundaries

The `shell` tool runs Bash through pipes in the writable current directory. On
Linux, Bubblewrap exposes selected read-only system and Nix store paths while
hiding other host files and giving each command private temporary directories.
On macOS, `sandbox-exec` leaves host files readable but limits ordinary
filesystem writes to fixed writable roots. The workspace's `.a` is protected on
both platforms except for sandbox state: `HOME` persists in `.a/home`, macOS
temporary files use `.a/tmp`, and active subagent transcript directories are
writable.

## Grant access to additional paths

Repeat `-r PATH` or `-rw PATH` to expose an existing host file or directory at
the same absolute path, read-only or read-write. Relative paths start at the
workspace. `-~ PATH` exposes an existing path beneath the host home read-write
at the same relative path beneath the sandbox home; for example,
`-~ .config/gh` lets `gh` find and update its host configuration. This creates a
durable symlink in `.a/home`; a conflicting entry is an error. All three flags
may be repeated. On macOS, where host files are already readable, `-r` adds no
access. These flags follow symlinks and grant the model access to the resulting
host data.

## Environment and network

Shell and dynamic-loader startup variables are removed; other environment
variables are shared. The host network and Nix daemon, when present, are
available. Existing mounts beneath the workspace remain visible. Use
`nix develop .#default -c COMMAND` for the current flake or
`nix shell nixpkgs#PACKAGE -c COMMAND` for an extra program.

## Running commands and collecting output

Commands normally return when they exit. After 20 seconds or 4 KiB of output,
the result instead reports a command ID. `shell_read` immediately consumes the
oldest buffered output for that ID, and `shell_write` queues exact stdin bytes
or EOF without adding a newline. `shell_wait` accepts a non-empty array of
unique command IDs and waits for up to five minutes. The array may be any subset
of unretired command IDs; unlisted commands are unaffected. It returns and
retires every listed command that is complete when any one exits, leaving the
others running; if the timeout expires first, it reports which listed commands
remain running without retiring them. An unknown ID makes the call fail without
consuming any output.
Unread output and pending stdin are each bounded to 4 KiB; the oldest unread
output is discarded and reported. These are plain pipes, not a terminal. IDs
expire when the assistant finishes the turn; live process groups receive
`SIGTERM` then `SIGKILL` after 20 seconds without blocking the next turn.

## Approve commands outside the sandbox

With terminal input, the model also gets a `[y/N]`-approved `host_shell` tool.
It shows the exact Bash command before running it outside the sandbox with
workspace `PATH` entries and shell or dynamic-loader startup variables removed.
Every non-empty `shell_write` to a host command requires another approval
showing the input in a block; reads and EOF-only closes do not.

## Limitations and logging

Linked worktrees whose Git metadata is outside the workspace are unsupported;
ordinary clones work. `host_shell` requires terminal input. Calls from one
model response run concurrently, with output and status returned in completion
order. Tool activity is summarized on stderr; full details and output are kept
temporarily in `<transcript>.live`. `-n` disables all tools.

