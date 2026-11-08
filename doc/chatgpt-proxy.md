# ChatGPT adapter

`a-chatgpt-proxy` connects `a` to a ChatGPT plan through
[Sign in with ChatGPT](https://developers.openai.com/siwc/token-sharing-open-source).
It implements model listing and the streaming Chat Completions subset used by
`a`, not the full OpenAI API.

## Sign in and start

[Install both programs](../README.md#install), then run:

```sh
a-chatgpt-proxy login   # open the printed link in a browser
a-chatgpt-proxy         # leave running on 127.0.0.1:8080
```

In another terminal, list models and send a prompt:

```sh
a -l
a -n -m MODEL what is tail recursion?
```

Replace `MODEL` with an ID from the list. `-n` disables tools. To work on files,
enter your project directory and omit `-n`; tools can modify that workspace.
See [Tools and security](tools-and-security.md) for access boundaries.

## Change the port

The default port is 8080. Set a different port on the adapter and a matching
URL on the client:

```sh
a-chatgpt-proxy -p 8081
# in another terminal:
a -u http://127.0.0.1:8081 -l
```

## Credentials and separate accounts

Credentials default to `~/.a-chatgpt-proxy/credentials`. Use `-c FILE`
consistently to keep another account in a separate file. Options go before
`login` or `logout`:

```sh
a-chatgpt-proxy -c /path/to/credentials login
a-chatgpt-proxy -c /path/to/credentials
```

Credential files and their parent directories are created with owner-only
permissions. The stable host identifier is stored in `~/.a-chatgpt-proxy/host`.

## Stop and log out

Stop the running adapter, then log out:

```sh
a-chatgpt-proxy logout
```

For a custom credential file, use `a-chatgpt-proxy -c FILE logout`.

## Reasoning state

The adapter always requests an automatic reasoning summary. It keeps opaque
reasoning items in memory only for the duration of an active tool loop.
Restarting it during that loop makes the continuation fail rather than
silently discard the reasoning state.
