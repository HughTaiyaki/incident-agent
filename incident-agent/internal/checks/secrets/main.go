// Command secrets rejects credentials in files Git tracks or would add. The repository
// is public, so a credential that reaches a commit has to be treated as leaked.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var patterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"API key", regexp.MustCompile(`\bsk-(?:ant-)?[A-Za-z0-9_-]{20,}`)},
	{"GitHub token", regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)},
	{"AWS key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"bearer token", regexp.MustCompile(`(?i)authorization:\s*bearer\s+[A-Za-z0-9._~+/-]{24,}`)},
}

// findings reports locations only; repeating a secret in output would spread it further.
func findings(name string, data []byte) []string {
	if bytes.IndexByte(data, 0) >= 0 {
		return nil
	}
	found := []string{}
	for _, pattern := range patterns {
		if location := pattern.re.FindIndex(data); location != nil {
			found = append(found, fmt.Sprintf("%s:%d: %s", name, 1+bytes.Count(data[:location[0]], []byte("\n")), pattern.name))
		}
	}
	return found
}

func main() {
	output, err := exec.CommandContext(context.Background(), "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "list files:", err)
		os.Exit(1)
	}
	failed := false
	for _, name := range strings.Split(string(output), "\x00") {
		if name == "" {
			continue
		}
		// #nosec G304 -- Paths come from git in the repository being checked.
		data, err := os.ReadFile(name)
		if err != nil {
			if info, statErr := os.Lstat(name); statErr == nil && info.Mode()&fs.ModeSymlink != 0 {
				continue
			}
			if os.IsNotExist(err) {
				continue
			}
			_, _ = fmt.Fprintln(os.Stderr, "read file:", err)
			os.Exit(1)
		}
		for _, finding := range findings(name, data) {
			failed = true
			_, _ = fmt.Fprintln(os.Stderr, finding)
		}
	}
	if failed {
		_, _ = fmt.Fprintln(os.Stderr, "credentials found; remove them before committing and revoke any that were published")
		os.Exit(1)
	}
}
