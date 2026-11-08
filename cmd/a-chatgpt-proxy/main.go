package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	defaultPort    = 8080
	manageUsageURL = "https://chatgpt.com/settings/usage"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(stderr, fmt.Errorf("find home directory: %w", err))
	}
	defaultDirectory := filepath.Join(home, ".a-chatgpt-proxy")

	flags := flag.NewFlagSet("a-chatgpt-proxy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: a-chatgpt-proxy [options] [login|logout]")
		flags.PrintDefaults()
	}
	port := flags.Int("p", defaultPort, "listen on 127.0.0.1:`PORT`")
	credentialsPath := flags.String("c", filepath.Join(defaultDirectory, "credentials"), "read and write credentials in `FILE`")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *port < 1 || *port > 65535 {
		return fail(stderr, fmt.Errorf("invalid port %d", *port))
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return 2
	}

	path := *credentialsPath
	identity := newAuth(path, filepath.Join(defaultDirectory, "host"), stderr)
	command := ""
	if flags.NArg() == 1 {
		command = flags.Arg(0)
	}

	switch command {
	case "login":
		previous, readErr := readCredentials(path)
		first := readErr != nil || !hasScope(previous.Scopes, directScope)
		email, err := identity.login(context.Background())
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stderr, "a-chatgpt-proxy: signed in as %s [%s]\n", email, path)
		if first {
			fmt.Fprintf(stderr, "a-chatgpt-proxy: eligible requests use your ChatGPT plan; manage usage at %s\n", manageUsageURL)
		}
		return 0

	case "logout":
		email, err := identity.account()
		if err != nil {
			return fail(stderr, err)
		}
		if err := identity.logout(context.Background()); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stderr, "a-chatgpt-proxy: signed out %s [%s]\n", email, path)
		return 0

	case "":
		if _, err := identity.token(context.Background()); err != nil {
			return fail(stderr, err)
		}
		email, err := identity.account()
		if err != nil {
			return fail(stderr, err)
		}
		address := net.JoinHostPort("127.0.0.1", strconv.Itoa(*port))
		listener, err := net.Listen("tcp4", address)
		if err != nil {
			return fail(stderr, fmt.Errorf("listen on %s: %w", address, err))
		}
		defer listener.Close()
		fmt.Fprintf(stderr, "a-chatgpt-proxy: using ChatGPT plan as %s [%s]\n", email, path)
		fmt.Fprintf(stderr, "a-chatgpt-proxy: listening on http://%s\n", address)
		fmt.Fprintf(stderr, "a-chatgpt-proxy: manage usage at %s\n", manageUsageURL)
		handler := newProxy(identity, stderr)
		server := &http.Server{Handler: handler, ErrorLog: handler.errorLog, ReadHeaderTimeout: 5 * time.Second}
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fail(stderr, fmt.Errorf("serve: %w", err))
		}
		return 0

	default:
		flags.Usage()
		return 2
	}
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "a-chatgpt-proxy: %v\n", err)
	return 1
}
