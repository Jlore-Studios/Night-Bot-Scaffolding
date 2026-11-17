// Package gitx runs git in a checkout.
package gitx

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
)

// Repo is a checkout.
type Repo struct {
	Dir string
	// Env is added to git's environment (an auth header for a push, say).
	Env []string
}

// Git runs git with args and returns its trimmed stdout.
func (r *Repo) Git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(cmd.Environ(), r.Env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(redact(args), " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// MustGit is Git for calls that only fail when the checkout is broken.
func (r *Repo) MustGit(args ...string) string {
	out, err := r.Git(args...)
	if err != nil {
		panic(err)
	}
	return out
}

func redact(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.Contains(strings.ToLower(a), "authorization") || strings.Contains(a, "x-access-token") {
			a = "[redacted]"
		}
		out[i] = a
	}
	return out
}

// Lines splits git's output into its non-empty lines.
func Lines(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// Changed lists the paths that differ between base and head, renames as both their paths.
func (r *Repo) Changed(base, head string) ([]string, error) {
	out, err := r.Git("diff", "--name-only", "--no-renames", base, head, "--")
	if err != nil {
		return nil, err
	}
	return Lines(out), nil
}

// Dirty lists the paths the working tree changed since HEAD, tracked or not (ignored files are
// left out).
func (r *Repo) Dirty() ([]string, error) {
	out, err := r.Git("status", "--porcelain", "--untracked-files=all", "--no-renames")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 3 {
			path := line[3:]
			path = strings.Trim(path, `"`)
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// Exists says whether a revision exists.
func (r *Repo) Exists(rev string) bool {
	_, err := r.Git("rev-parse", "--verify", "--quiet", rev+"^{commit}")
	return err == nil
}

// AuthEnv has git authenticate to server with token, through the same extra header
// actions/checkout uses, without writing the token to any file or command line. It is set on the
// harness's own git commands only; the model's environment never has it.
func AuthEnv(server, token string) []string {
	if token == "" {
		return nil
	}
	if server == "" {
		server = "https://github.com"
	}
	header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http." + strings.TrimRight(server, "/") + "/.extraheader",
		"GIT_CONFIG_VALUE_0=" + header, "GIT_TERMINAL_PROMPT=0"}
}
