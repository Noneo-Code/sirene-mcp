// Command sirene-mcp is a Model Context Protocol server over a local DuckDB
// copy of the INSEE Sirene register.
package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
)

// version is set at build time with -ldflags "-X main.version=…" (release
// binaries); for `go install …@vX.Y.Z` it falls back to the module version.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = strings.TrimPrefix(info.Main.Version, "v")
	}
}

const usage = `sirene-mcp - MCP server over a local DuckDB copy of the INSEE Sirene register

Usage:
  sirene-mcp sync   [--ul-url URL] [--etab-url URL]  Download the stock files and rebuild the local database
                    [--exclude-closed]               Skip closed establishments and legal units
  sirene-mcp stdio                                   Run the MCP server over stdio
  sirene-mcp serve  [--http 127.0.0.1:8766]          Run the MCP server over Streamable HTTP (bearer token)
                    [--allow-remote]                 Allow a non-loopback address (put it behind TLS)
                    [--auto-sync]                    Refresh the database on a schedule
  sirene-mcp token                                   Print the bearer token used by serve
  sirene-mcp status                                  Show the database and last sync
  sirene-mcp version

Global flags (put them after the command):
  --data-dir DIR    Data directory (default: $SIRENE_MCP_DATA_DIR or <user cache dir>/sirene-mcp)
  --log-level LVL   debug, info, warn or error

Run "sirene-mcp <command> --help" for details.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sirene-mcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("missing command")
	}
	cmd := args[0]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("sirene-mcp", version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}
