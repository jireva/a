package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultServer = "http://127.0.0.1:8080"

type app struct {
	stdin         io.Reader
	stdout        io.Writer
	stderr        io.Writer
	stdinTerminal bool
	started       int64
	server        string
	model         string
	effort        string
	noTools       bool
	ultra         bool
	subagent      bool
	mounts        []sandboxMount
	transcriptDir string
	conversation  *conversation
}

func main() {
	started := time.Now().UnixNano()
	stdinInfo, err := os.Stdin.Stat()
	if err != nil {
		fmt.Fprintf(os.Stderr, "… inspect stdin: %v\n", err)
		os.Exit(1)
	}

	a := app{
		stdin:         os.Stdin,
		stdout:        os.Stdout,
		stderr:        os.Stderr,
		stdinTerminal: stdinInfo.Mode()&os.ModeCharDevice != 0,
		started:       started,
		server:        defaultServer,
		model:         os.Getenv("LLAMA_ARG_MODEL"),
		transcriptDir: os.Getenv("A_TRANSCRIPT_DIR"),
	}
	os.Exit(a.run(os.Args[1:]))
}

func (a *app) run(args []string) (code int) {
	defer func() {
		if a.conversation == nil {
			return
		}
		if err := a.conversation.closeLive(); err != nil {
			fmt.Fprintf(a.stderr, "… %v\n", err)
			code = 1
		}
	}()

	flags := flag.NewFlagSet("a", flag.ContinueOnError)
	flags.SetOutput(a.stderr)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: a [options] [prompt ...]")
		flags.PrintDefaults()
	}
	list := flags.Bool("l", false, "list models")
	server := flags.String("u", a.server, "use chat server base `URL`")
	model := flags.String("m", a.model, "request model `MODEL`")
	transcript := flags.String("t", "", "create or continue transcript `FILE`")
	effort := flags.String("e", "", "set reasoning effort to `EFFORT`")
	noTools := flags.Bool("n", false, "disable shell tools")
	ultra := flags.Bool("U", false, "delegate work proactively to subagents")
	subagent := flags.Bool("s", false, "run as a subagent")
	var readOnlyMounts, readWriteMounts, homeMounts []string
	flags.Func("r", "allow read-only access to `PATH`", func(path string) error {
		readOnlyMounts = append(readOnlyMounts, path)
		return nil
	})
	flags.Func("rw", "allow read-write access to `PATH`", func(path string) error {
		readWriteMounts = append(readWriteMounts, path)
		return nil
	})
	flags.Func("~", "mount host-home `PATH` read-write in sandbox HOME", func(path string) error {
		homeMounts = append(homeMounts, path)
		return nil
	})
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	a.server = *server
	a.model = *model
	a.effort = *effort
	a.noTools = *noTools
	a.ultra = *ultra
	a.subagent = *subagent
	mounts, err := prepareSandboxMounts(readOnlyMounts, readWriteMounts, homeMounts)
	if err != nil {
		return a.fail(err)
	}
	a.mounts = mounts
	if *list {
		if err := listModels(a.server, a.stdout); err != nil {
			return a.fail(err)
		}
		return 0
	}

	argumentPrompt := strings.Join(flags.Args(), " ")
	if a.stdinTerminal {
		in := bufio.NewReader(a.stdin)
		if argumentPrompt != "" {
			return a.oneTurn(argumentPrompt, *transcript, in)
		}
		return a.repl(*transcript, in)
	}

	input, err := io.ReadAll(a.stdin)
	if err != nil {
		return a.fail(fmt.Errorf("read stdin: %w", err))
	}
	prompt := string(input)
	if len(input) != 0 && argumentPrompt != "" {
		prompt += "\n\n\n"
	}
	prompt += argumentPrompt
	if prompt == "" {
		return 0
	}
	return a.oneTurn(prompt, *transcript, nil)
}

func (a *app) oneTurn(prompt, name string, hostInput *bufio.Reader) int {
	c, err := a.newConversation(name, hostInput)
	if err != nil {
		return a.fail(err)
	}
	if _, err := a.turn(c, prompt); err != nil {
		return a.fail(err)
	}
	return 0
}

func (a *app) repl(name string, in *bufio.Reader) int {
	var c *conversation
	var lines []string

	for {
		prompt := "> "
		if len(lines) != 0 {
			prompt = "  "
		}
		if _, err := io.WriteString(a.stderr, prompt); err != nil {
			return 1
		}
		line, readErr := in.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return a.fail(fmt.Errorf("read stdin: %w", readErr))
		}
		line = strings.TrimSuffix(line, "\n")
		if readErr == nil && strings.HasSuffix(line, `\`) {
			lines = append(lines, strings.TrimSuffix(line, `\`))
			continue
		}
		if len(lines) != 0 {
			lines = append(lines, line)
			line = strings.Join(lines, "\n")
			lines = nil
		}

		if line == "" {
			if errors.Is(readErr, io.EOF) {
				if _, err := io.WriteString(a.stderr, "\n"); err != nil {
					return 1
				}
				return 0
			}
			continue
		}

		if c == nil {
			var err error
			c, err = a.newConversation(name, in)
			if err != nil {
				return a.fail(err)
			}
		}
		if _, err := io.WriteString(a.stderr, "\n"); err != nil {
			return 1
		}
		_, err := a.turn(c, line)
		if err != nil {
			return a.fail(err)
		}

		if errors.Is(readErr, io.EOF) {
			return 0
		}
	}
}

func (a *app) turn(c *conversation, prompt string) (string, error) {
	out := a.stdout
	reasoningOut := a.stderr
	if a.subagent {
		out = io.Discard
		reasoningOut = io.Discard
	}
	response, err := c.turn(prompt, out, reasoningOut)
	if err != nil {
		return "", err
	}
	finalResponse := c.messages[len(c.messages)-1].Content
	if a.subagent {
		response = finalResponse
		if _, err := io.WriteString(a.stdout, response); err != nil {
			return "", fmt.Errorf("write response: %w", err)
		}
	}
	if finalResponse != "" && !strings.HasSuffix(finalResponse, "\n") {
		if _, err := io.WriteString(a.stderr, "\n"); err != nil {
			return "", fmt.Errorf("write assistant address: %w", err)
		}
	}
	if _, err := fmt.Fprintf(a.stderr, "  %s:%d\n\n", c.path, c.assistantLine); err != nil {
		return "", fmt.Errorf("write assistant address: %w", err)
	}
	return response, nil
}

func (a *app) newConversation(name string, hostInput *bufio.Reader) (*conversation, error) {
	auto := name == ""
	if auto {
		var err error
		name, err = a.nextTranscript()
		if err != nil {
			return nil, err
		}
	}
	c := &conversation{
		name:           name,
		server:         a.server,
		model:          a.model,
		effort:         a.effort,
		noTools:        a.noTools,
		ultra:          a.ultra,
		subagent:       a.subagent,
		mounts:         a.mounts,
		hostInput:      hostInput,
		status:         a.stderr,
		nextTranscript: a.nextTranscript,
	}
	a.conversation = c
	return c, nil
}

func (a *app) nextTranscript() (string, error) {
	dir := a.transcriptDir
	if dir == "" {
		dir = filepath.Join(".a", "transcripts")
	}
	if err := os.MkdirAll(dir, 0777); err != nil {
		return "", fmt.Errorf("create transcript directory %q: %w", dir, err)
	}
	for {
		name := filepath.Join(dir, strconv.FormatInt(a.started, 10))
		a.started++
		if _, err := os.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", fmt.Errorf("inspect transcript %q: %w", name, err)
		}
	}
}

func (a *app) fail(err error) int {
	fmt.Fprintf(a.stderr, "… %v\n", err)
	return 1
}
