package main

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

var shellTool = toolDefinition{
	name:        "shell",
	description: "Run a Bash command in the current directory. The sandbox makes the workspace writable, limits host writes, hides ungranted host paths, and shares the network. If the command is still running after 20 seconds or 4 KiB of output, its result reports an ID for shell_read, shell_write, and shell_wait. This is a pipe, not a terminal. Use `nix develop .#default -c ...` for the current flake or `nix shell nixpkgs#PACKAGE -c ...` for an extra program. For multi-file edits, use `git apply --recount --whitespace=nowarn -` with a standard Git diff; do not invoke `apply_patch`. Multiple shell calls in one response run concurrently; combine dependent commands into one call.",
	parameters:  shellParameters,
}

var hostShellTool = toolDefinition{
	name:        "host_shell",
	description: "Request a Bash command outside the sandbox. The exact command is shown to the user and requires a separate [y/N] approval every time. Long-running commands use the same shell_read, shell_write, and shell_wait interface as shell commands, and every non-empty write requires approval.",
	parameters:  shellParameters,
}

var shellReadTool = toolDefinition{
	name:        "shell_read",
	description: "Read and consume the oldest currently buffered output from a running shell command. Returns immediately, and reports whether the command is still running or has exited.",
	parameters:  shellReadParameters,
}

var shellWriteTool = toolDefinition{
	name:        "shell_write",
	description: "Queue exact bytes for a running shell command's stdin, optionally followed by EOF. Adds no newline. At least one of non-empty input or close=true is required.",
	parameters:  shellWriteParameters,
}

var shellWaitTool = toolDefinition{
	name:        "shell_wait",
	description: "Wait up to five minutes for at least one listed shell or host_shell command to exit. Return and retire every listed command that has exited while leaving the rest running. On timeout, report each listed command still running without retiring it. Give a non-empty list containing only the distinct, unretired command IDs you want to wait for. Do not call shell_wait concurrently.",
	parameters:  shellWaitParameters,
}

var shellParameters = json.RawMessage(`{
	"type":"object",
	"properties":{"command":{"type":"string","description":"Bash source to execute."}},
	"required":["command"],
	"additionalProperties":false
}`)

var shellReadParameters = json.RawMessage(`{
	"type":"object",
	"properties":{"command_id":{"type":"string","description":"ID reported by a running shell command."}},
	"required":["command_id"],
	"additionalProperties":false
}`)

var shellWriteParameters = json.RawMessage(`{
	"type":"object",
	"properties":{
		"command_id":{"type":"string","description":"ID reported by a running shell command."},
		"input":{"type":"string","description":"Exact bytes to queue; no newline is added. Use an empty string when only closing stdin."},
		"close":{"type":"boolean","description":"Whether to queue EOF after all accepted input."}
	},
	"required":["command_id","input","close"],
	"additionalProperties":false
}`)

var shellWaitParameters = json.RawMessage(`{
	"type":"object",
	"properties":{
		"command_ids":{
			"type":"array",
			"items":{"type":"string"},
			"description":"Distinct, unretired IDs of the shell or host_shell commands you want to wait for."
		}
	},
	"required":["command_ids"],
	"additionalProperties":false
}`)

type shellWriteArguments struct {
	CommandID string
	Input     string
	Close     bool
}

func decodeShellArguments(raw string) (string, error) {
	var arguments struct {
		Command *string `json:"command"`
	}
	if err := decodeArguments(raw, &arguments); err != nil {
		return "", err
	}
	if arguments.Command == nil {
		return "", errors.New("command must be a string")
	}
	return *arguments.Command, nil
}

func decodeShellReadArguments(raw string) (string, error) {
	var arguments struct {
		CommandID *string `json:"command_id"`
	}
	if err := decodeArguments(raw, &arguments); err != nil {
		return "", err
	}
	if arguments.CommandID == nil || *arguments.CommandID == "" {
		return "", errors.New("command_id must be a non-empty string")
	}
	return *arguments.CommandID, nil
}

func decodeShellWriteArguments(raw string) (shellWriteArguments, error) {
	var arguments struct {
		CommandID *string `json:"command_id"`
		Input     string  `json:"input"`
		Close     bool    `json:"close"`
	}
	if err := decodeArguments(raw, &arguments); err != nil {
		return shellWriteArguments{}, err
	}
	if arguments.CommandID == nil || *arguments.CommandID == "" {
		return shellWriteArguments{}, errors.New("command_id must be a non-empty string")
	}
	if arguments.Input == "" && !arguments.Close {
		return shellWriteArguments{}, errors.New("input must be non-empty or close must be true")
	}
	return shellWriteArguments{CommandID: *arguments.CommandID, Input: arguments.Input, Close: arguments.Close}, nil
}

func decodeShellWaitArguments(raw string) ([]string, error) {
	var arguments struct {
		CommandIDs []string `json:"command_ids"`
	}
	if err := decodeArguments(raw, &arguments); err != nil {
		return nil, err
	}
	if len(arguments.CommandIDs) == 0 {
		return nil, errors.New("command_ids must be a non-empty array")
	}
	seen := make(map[string]bool, len(arguments.CommandIDs))
	for _, id := range arguments.CommandIDs {
		if id == "" {
			return nil, errors.New("command_ids must contain non-empty strings")
		}
		if seen[id] {
			return nil, errors.New("command_ids must contain unique strings")
		}
		seen[id] = true
	}
	return arguments.CommandIDs, nil
}

func decodeArguments(raw string, arguments any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(arguments); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("arguments contain more than one JSON value")
		}
		return err
	}
	return nil
}
