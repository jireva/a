# Model servers

`a` needs a server that supports the model-listing and streaming Chat
Completions endpoints it uses.

- Server: `http://127.0.0.1:8080` by default; override with `-u URL`.
- Model: `$LLAMA_ARG_MODEL` by default; override with `-m MODEL`.
- Use `-l` to list model IDs on the selected server.

Give `-u` a base URL. `a` appends `/v1/chat/completions` for requests and
`/v1/models` for model listing.

## ChatGPT

The local `a-chatgpt-proxy` adapter connects a ChatGPT plan. See the
[adapter guide](chatgpt-proxy.md) for setup, credentials, ports, and logout.

## Local llama

The flake's development shell provides a local llama playground. The example
assumes `a` is already installed. Enter the development shell in both
terminals so that the server and client share its defaults:

```sh
# first terminal
nix develop github:jireva/a
llama serve
```

```sh
# second terminal
nix develop github:jireva/a
a what is tail recursion?
```

Entering this shell downloads the large Qwen3-4B Q4_K_M model. The shell sets
`LLAMA_ARG_MODEL` to that model, which `llama serve` loads and `a` uses by
default.

## PPQ

To use the PPQ privacy verifier, run its proxy independently in a separate
terminal:

```sh
PPQ_API_KEY=... nix run github:jireva/a#ppq-private-proxy
```

Then connect `a` to it:

```sh
a -u http://127.0.0.1:8787 -l
a -u http://127.0.0.1:8787 -m private/gpt-oss-120b your prompt
```

## Other servers

Use `-u` and `-m` with another server that provides the model-listing and
streaming Chat Completions endpoints expected by `a`:

```sh
a -u http://server:port -l
a -u http://server:port -m MODEL your prompt
```

