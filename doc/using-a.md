# Using `a`

Set up a [model server](model-servers.md) first. Replace `MODEL` below with an
ID returned by `a -l`. Put flags **before** the prompt.

## Send a prompt

```sh
a -l
a -m MODEL what is tail recursion?
a -n -m MODEL answer without tools
```

Tools are enabled by default. Use `-n` to prevent the model from running commands.

## Have a conversation

With terminal input and no prompt arguments, `a` starts an interactive REPL:

```sh
a -m MODEL
```

Enter a prompt at `> `. End a line with a backslash (`\`) to continue it on the
next line. Send EOF (usually Ctrl-D) to exit. Conversation history is retained
between turns.

To create or continue a named conversation:

```sh
a -m MODEL -t notes.a
a -m MODEL -t notes.a next question
```

Conversations are saved automatically even without `-t`. See [Transcripts](transcripts.md).

## Pipe text into a prompt

```sh
git diff | a -m MODEL review this
```

With non-terminal stdin, `a` reads all input and makes one turn. If stdin and
prompt arguments are both non-empty, it joins them as: stdin, three newlines,
then the prompt arguments.

## Command-line reference

```text
a [-U] [-s] [-l] [-n] [-r path] [-rw path] [-~ home-path] [-t transcript] [-u server] [-m model] [-e effort] [prompt ...]
```

| Flag | Meaning |
| --- | --- |
| `-u URL` | Server base URL; defaults to `http://127.0.0.1:8080` |
| `-m MODEL` | Model ID; defaults to `$LLAMA_ARG_MODEL` |
| `-l` | List model IDs and exit |
| `-n` | Disable all tools |
| `-t FILE` | Create or continue a transcript |
| `-e EFFORT` | Send this value as `reasoning_effort`, for example `-e high` |
| `-r PATH` | Grant read-only access to an existing host path; repeatable |
| `-rw PATH` | Grant read-write access to an existing host path; repeatable |
| `-~ PATH` | Expose a host-home path read-write in the sandbox home; repeatable |
| `-U` | Enable proactive delegation to subagents |
| `-s` | Run in subagent mode; shell commands are **unsandboxed when invoked directly** |

See [Tools and security](tools-and-security.md) for permissions and
[Subagents](subagents.md) for delegation.

## Output and requests

Assistant responses stream unchanged to stdout. Reasoning, when supplied by
the server, streams to stderr, with `~ ` at the start of each block. Tool
activity and transcript addresses also go to stderr.

`a` appends `/v1/chat/completions` to the server URL and sends the full current
conversation. `-l` fetches `/v1/models` and prints each `data[].id` on its own
line. Without `-e`, the `reasoning_effort` field is omitted.
