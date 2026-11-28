package keychain

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
)

// ReadGitHubToken imports the token for host from gh. gh selects credentials by hostname.
func ReadGitHubToken(ctx context.Context, host string) (Secret, error) {
	args := []string{"auth", "token"}
	if host != "" {
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		args = append(args, "--hostname", host)
	}
	out, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		return Secret{}, errors.New("gh auth token failed; run gh auth login")
	}
	return FromString(string(out))
}

// ReadJiraCredential prompts for the account email and token and returns a single basic-auth Secret. One reader
// owns both lines because reading the email from a pipe may also buffer the token.
func ReadJiraCredential(ctx context.Context, r io.Reader, prompts io.Writer) (Secret, error) {
	in := bufio.NewReader(r)
	fmt.Fprint(prompts, "Atlassian account email: ")
	email, err := in.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && email != "") {
		return Secret{}, err
	}
	fmt.Fprint(prompts, "API token (input hidden): ")
	token, err := readSecret(ctx, r, in)
	fmt.Fprintln(prompts)
	if err != nil {
		return Secret{}, err
	}
	return PairWith(email, token)
}

// echoOff turns terminal echo off for r and returns the function that restores it. Tests replace it.
var echoOff = noEcho

func noEcho(r io.Reader) func() {
	if f, ok := r.(*os.File); ok {
		if restore, err := hideEcho(f); err == nil {
			return restore
		}
	}
	return func() {}
}

// ReadSecret reads one line from r as a secret, trimming surrounding space. When r is a terminal, echo is off while
// it reads, and is restored before ReadSecret returns, also when ctx ends first (an interrupt): then it returns the
// context's error without waiting for the line. An empty line is an error.
func ReadSecret(ctx context.Context, r io.Reader) (Secret, error) {
	return readSecret(ctx, r, bufio.NewReader(r))
}

func readSecret(ctx context.Context, r io.Reader, in *bufio.Reader) (Secret, error) {
	restore := echoOff(r)
	defer restore()
	return readSecretLine(ctx, in)
}

func readSecretLine(ctx context.Context, in *bufio.Reader) (Secret, error) {
	type read struct {
		line string
		err  error
	}
	done := make(chan read, 1)
	go func() {
		line, err := in.ReadString('\n')
		done <- read{line, err}
	}()
	var line string
	var err error
	select {
	case <-ctx.Done():
		return Secret{}, ctx.Err()
	case got := <-done:
		line, err = got.line, got.err
	}
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		if errors.Is(err, io.EOF) {
			return Secret{}, errors.New("keychain: no secret given")
		}
		return Secret{}, err
	}
	v := strings.TrimSpace(line)
	if v == "" {
		return Secret{}, errors.New("keychain: no secret given")
	}
	return Secret{value: []byte(v)}, nil
}
