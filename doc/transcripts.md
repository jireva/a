# Transcripts and compaction

`a` saves successful turns so you can inspect or continue a conversation later.

## Save and continue

Without `-t`, transcripts are saved under `./.a/transcripts/`. To choose a file
or continue an existing conversation:

```sh
a -m MODEL -t notes.a explain this project
a -m MODEL -t notes.a which part should I read first?
```

Replace `MODEL` with your model ID. After each successful turn, stderr shows
the final assistant message's address as `<absolute path>:<line>`.

## File format

Transcripts are plain UTF-8 text:

```text
user:
Hello
.
assistant:
Hi
.
```

Successful shell calls are recorded in chronological order:

```text
shell tool:
printf 'hello\n'
.
output:
hello
exit status: 0
.
```

Reasoning is sent back only within its active tool loop and is not stored in
transcripts. A body line made entirely of `.`s is escaped with one extra `.`.

## Automatic compaction

Before each model request, `a` checks the encoded request size. If it would
exceed **400000 bytes**, `a`:

1. Asks the same model for a checkpoint, with tools disabled.
2. Closes the current transcript with a bare final `compaction:` line.
3. Continues in a new automatically named transcript.

The new file starts with a `summary:` record containing the checkpoint and
the absolute path of the old file. Both paths are printed to stderr. This
also applies when you selected the original transcript with `-t`.

Continuing a closed transcript with `-t` creates a new branch from it rather
than appending to the closed file.
