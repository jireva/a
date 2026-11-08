# Subagents

Use `-U` to let the assistant proactively delegate work to other instances of `a`:

```sh
a -U -m MODEL implement this feature
```

Replace `MODEL` with your model ID. The root agent's sandboxed `shell` gets an
`a` function that starts subagents with the same server, model, and reasoning effort.

## How subagents run

Subagents run with `-s`. They:

- Receive the bundled subagent instructions.
- Run shell commands inside the root agent's existing sandbox.
- Print only their final response and its transcript address.
- Save new transcripts in `<root-transcript>.s/`.

See [Transcripts](transcripts.md) for the file format and
[Tools and security](tools-and-security.md) for sandbox limits.

## Direct invocation is not sandboxed

**Invoking `a -s` directly runs its shell commands without a sandbox.** Subagent
mode relies on the root agent's sandbox when launched through the injected
`a` function. For normal delegation, use `-U`, not `-s`.
