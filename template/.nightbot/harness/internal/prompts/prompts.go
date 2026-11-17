// Package prompts renders the templates in .nightbot/prompts/ and fences text that came from
// GitHub, so the model reads it as data rather than as instructions.
package prompts

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"nightbot/internal/config"
)

// Render fills the template name (system, build, fix, review, ask, triage) with vars. A
// template reads a variable as {{.name}}; one it names but vars lacks is empty.
func Render(cfg *config.Config, name string, vars map[string]string) (string, error) {
	path := filepath.Join(cfg.Root, config.Dir, "prompts", name+".md")
	text, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tmpl, err := template.New(name).Option("missingkey=zero").Parse(string(text))
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	data := map[string]string{"repo": cfg.Repo, "default_branch": cfg.DefaultBranch,
		"bot_login": cfg.BotLogin, "command_prefix": cfg.CommandPrefix}
	for k, v := range vars {
		data[k] = v
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return out.String(), nil
}

const (
	fenceOpen  = "<untrusted-data"
	fenceClose = "</untrusted-data>"
)

// Fence wraps text from outside (issue bodies, comments, CI logs, another model's report) as
// data. Anything inside that looks like the fence's end is defused, so the text cannot close the
// fence early and speak as the harness.
func Fence(source, text string) string {
	text = strings.ReplaceAll(text, fenceClose, "</untrusted-data\u200b>")
	text = strings.ReplaceAll(text, fenceOpen, "<untrusted-data\u200b")
	return fmt.Sprintf("%s source=%q>\n%s\n%s", fenceOpen, source, strings.TrimRight(text, "\n"), fenceClose)
}

// Truncate keeps the first n bytes of s (on a rune boundary), saying how much it cut.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n\n[… %d more bytes cut]", len(s)-cut)
}

// Tail keeps the last n bytes of s, saying how much it cut.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && (s[start]&0xC0) == 0x80 {
		start++
	}
	return fmt.Sprintf("[… %d earlier bytes cut]\n", start) + s[start:]
}
