// Command kb manages a git-backed, cross-checked knowledge bundle shared by
// coding agents (Claude Code, Codex, Hermes, Cursor, ...).
package main

import (
	"os"

	"github.com/xuehaipeng/agent-kb/internal/cli"
)

var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
