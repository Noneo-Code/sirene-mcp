# sirene-mcp

[![CI](https://github.com/Noneo-Code/sirene-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/Noneo-Code/sirene-mcp/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Noneo-Code/sirene-mcp.svg)](https://pkg.go.dev/github.com/Noneo-Code/sirene-mcp)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**A [Model Context Protocol](https://modelcontextprotocol.io) server over a local copy of the INSEE Sirene register.**
Let Claude (or any MCP client) search, count, look up and export French companies and
establishments, with no API quota and no network call at query time.

sirene-mcp downloads the monthly Sirene stock files (Parquet), builds a read-only
[DuckDB](https://duckdb.org) database on your machine, and serves it through MCP over stdio or
authenticated HTTP. One binary, no external service.

> [!NOTE]
> **Data source and licence.** Données INSEE – base Sirene, Licence Ouverte / Etalab 2.0.
> The stock files come from [data.gouv.fr](https://www.data.gouv.fr). sirene-mcp is an independent
> project, **not affiliated with or endorsed by INSEE**. The code itself is MIT-licensed.

## Quick start

**1. Install.** On macOS (Apple Silicon) or Linux, this downloads the latest release into `~/.local/bin`:

```sh
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
mkdir -p ~/.local/bin
curl -fsSL "https://github.com/Noneo-Code/sirene-mcp/releases/latest/download/sirene-mcp_${OS}_${ARCH}.tar.gz" \
  | tar -xz -C ~/.local/bin sirene-mcp
~/.local/bin/sirene-mcp version
```

Prebuilt binaries exist for linux amd64/arm64 and macOS arm64. Linux binaries need glibc 2.35 or
newer (Debian 12, Ubuntu 22.04 and later).

<details>
<summary>Other ways: Go, or a manual download</summary>

With Go 1.26+ and a C toolchain (DuckDB is linked through CGO), for instance on an Intel Mac:
`CGO_ENABLED=1 go install github.com/Noneo-Code/sirene-mcp/cmd/sirene-mcp@latest` (installs to `~/go/bin`).

Or download the archive for your system from the [releases page](https://github.com/Noneo-Code/sirene-mcp/releases)
and check it against `checksums.txt`. On macOS, a binary downloaded with a browser is
quarantined: run `xattr -d com.apple.quarantine sirene-mcp` once.
</details>

**2. Build the database.** This downloads the monthly Sirene stock files (~3 GiB) and builds the
local database (~10 GiB, a few minutes; see [Sync](#sync) for the requirements):

```sh
~/.local/bin/sirene-mcp sync
~/.local/bin/sirene-mcp status   # location, row counts, last sync
```

Run it again each month (INSEE publishes a new stock at the beginning of the month), or let
`serve --auto-sync` do it.

**3. Add the server to Claude Code.** Register it with its absolute path, so it works
whatever your `PATH` is when Claude starts it:

```sh
claude mcp add sirene --scope user -- ~/.local/bin/sirene-mcp stdio
claude mcp list   # sirene: ... ✓ Connected
```

Then restart Claude Code (or run `/mcp`). `--scope user` makes it available in every
project. To reach a shared server over the network instead, see [Streamable HTTP](#streamable-http).

<details>
<summary>Claude Desktop, Cursor and other MCP clients</summary>

Add this to the client's `mcpServers` configuration, with the absolute path of the binary
(`echo ~/.local/bin/sirene-mcp` prints it):

```json
{
  "mcpServers": {
    "sirene": {
      "command": "/Users/you/.local/bin/sirene-mcp",
      "args": ["stdio"]
    }
  }
}
```
</details>

**4. Ask.** *"How many physiotherapists are there in the Gard?"*, *"Export the active bakeries of
Montpellier with 10 to 49 employees"*, *"Which legal form and how many establishments does this SIREN have?"*.

To update the binary later, run the install command again; your database is kept.

## Sync


`sirene-mcp sync` downloads the *unités légales* and *établissements* stock files, builds a new
database next to the live one, checks it (row counts against the previous build), then swaps it in
atomically. A running server picks up the new file on its next query, and a failed sync leaves the
live database untouched.

```bash
sirene-mcp sync                       # everything, closed units and establishments included
sirene-mcp sync --exclude-closed      # skip closed units and establishments: a much smaller database
sirene-mcp sync --memory-limit 8GB    # DuckDB memory limit for the build (default 4GB)
sirene-mcp sync --keep-downloads      # keep the Parquet files after the build
```

Requirements (sizes in GiB, 2^30 bytes):

| | Full sync | `--exclude-closed` |
|---|---|---|
| Download | ~0.7 GiB + ~2.2 GiB Parquet (same files) | same |
| Rows | ~30.3M legal units, ~44.5M establishments | ~16.8M legal units, ~16.9M establishments |
| Disk (database) | ~9.6 GiB | ~4.3 GiB |
| Peak disk during a sync | ~35 GiB (old and new database coexist, plus the downloads and DuckDB temp files) | ~21 GiB |
| RAM | ~5 GiB peak RSS (default `--memory-limit 4GB`) | ~4.2 GiB peak RSS |
| Duration | ~3.5 min (download ~2 min, build ~1.5 min) | ~2 min (build ~30 s) |

Measured on the Nov 2026 stock, Apple M-series, 18 GiB RAM, on a fast home connection. Durations
are dominated by the download speed. On that stock, every active establishment has a NAF rév. 2
code and 99.9997 % also carry their NAF 2025 code.

Source URLs can be overridden with `--ul-url` / `--etab-url` (or `SIRENE_MCP_UL_URL` /
`SIRENE_MCP_ETAB_URL`), for instance to point at a mirror.

### Data directory

By default everything lives in `<user cache dir>/sirene-mcp` (`~/Library/Caches/sirene-mcp` on macOS,
`~/.cache/sirene-mcp` on Linux). Override it with `--data-dir DIR` or `SIRENE_MCP_DATA_DIR`
(the flag goes after the command). Exports are written to `<data dir>/exports` unless you pass
`--export-dir`. Large queries spill to `<data dir>/work`.

### Environment variables

Each one is a fallback for a flag; a flag given on the command line wins.

| Variable | Flag | Commands |
|---|---|---|
| `SIRENE_MCP_DATA_DIR` | `--data-dir` | all |
| `SIRENE_MCP_UL_URL`, `SIRENE_MCP_ETAB_URL` | `--ul-url`, `--etab-url` | `sync`, `serve --auto-sync` |
| `SIRENE_MCP_EXCLUDE_CLOSED` (`true`/`false`) | `--exclude-closed` | `sync`, `serve --auto-sync` |
| `SIRENE_MCP_MEMORY_LIMIT` (e.g. `4GB`) | `--memory-limit` | `sync` (build, default 4GB), `stdio` and `serve` (queries, default 80 % of RAM; `serve --auto-sync` also uses it for the build) |
| `SIRENE_MCP_EXPORT_DIR` | `--export-dir` | `stdio`, `serve` |
| `SIRENE_MCP_TOKEN` | | `serve`, `token`: bearer token, at least 16 characters |

## Connect a client

### stdio (local, recommended)

See [Quick start](#quick-start). With a custom data directory:

```bash
claude mcp add sirene --scope user --env SIRENE_MCP_DATA_DIR=/data/sirene -- ~/.local/bin/sirene-mcp stdio
```

Any MCP client that can launch a command works the same way: run `sirene-mcp stdio`.

### Streamable HTTP

For a client on another machine or a long-running shared server:

```bash
sirene-mcp serve                      # listens on 127.0.0.1:8766, MCP endpoint at /mcp
sirene-mcp serve --auto-sync          # also refreshes the database monthly (accepts the sync flags);
                                      # a failed sync is retried twice within about an hour

claude mcp add --transport http sirene http://127.0.0.1:8766/mcp \
  --header "Authorization: Bearer $(sirene-mcp token)"
```

`sirene-mcp token` prints the bearer token (generated on first use and stored in the data
directory; `SIRENE_MCP_TOKEN` overrides it and must be at least 16 characters). The server refuses non-loopback addresses unless you
pass `--allow-remote`; in that case put it behind a TLS reverse proxy.

## Tools

| Tool | What it does |
|---|---|
| `search_establishments` | List establishments matching filters (name, NAF, department, commune, postal code, size, legal category, creation date, head office only, active or closed...), paginated with a real `total`. To look up a SIREN or SIRET, use `get_company` |
| `count_establishments` | Count matches, optionally grouped by department, NAF code, headcount band or legal category |
| `get_company` | One legal unit by SIREN or SIRET: identity, head office and its establishments |
| `search_naf` | Find NAF codes by label, code prefix or common profession word (kiné, plombier... through a small curated alias list), in NAF rev. 2 or NAF 2025. Returns every match with the official INSEE label |
| `search_legal_categories` | Find legal category codes (SAS, SARL, association...); returns every match |
| `export_establishments` | Write every match to a CSV or JSON file on the server and return its path |
| `sql_query` | Custom read-only SQL (DuckDB dialect), a single `SELECT`/`WITH`, row-capped |
| `data_info` | Last sync date, row counts, sources and freshest record date |

### Partial diffusion

Some people (mostly sole proprietors) asked INSEE not to publish their details. Those rows are
flagged `diffusion_partielle = true` and the masked fields read `[ND]` (non diffusible). They are
counted and listed, but their address, name and similar fields are not available, and the server
tells the model so. Always mention it when reporting results.

## Security

- The database is opened **read-only**; tools cannot modify it.
- `sql_query` is **sandboxed**: one statement, `SELECT`/`WITH` only, no data-modifying CTEs, no file
  or network access, capped rows and a timeout.
- HTTP requires a **bearer token** on every request and listens on **loopback only** by default;
  `--allow-remote` is an explicit opt-in and should sit behind TLS.
- Exports are written only inside the export directory.
- Nothing is sent anywhere at query time; the only network access is `sync` fetching the stock files.

## Limitations

- **Monthly refresh.** INSEE publishes the stock files monthly; the data can be several weeks old
  (`data_info` reports the dates). Run `sync` (or `serve --auto-sync`) to refresh.
- **No enrichment.** Only what Sirene contains: no turnover, financial statements, emails, phone
  numbers, websites or managers.
- **Partial diffusion.** Establishments in *diffusion partielle* have masked fields (see above).
- A full database is large (see the requirements table); `--exclude-closed` trims it down.
- Binaries are built with CGO (DuckDB): linux amd64/arm64 (glibc 2.35+) and macOS arm64 only.

## Development

```bash
go vet ./...
go test ./...
golangci-lint run
```

CI runs tests on Linux and macOS, lints, and builds on linux amd64/arm64 and macOS arm64. Pushing a
`v*` tag builds the archives on native runners and publishes a GitHub release with checksums.

## License

MIT, see [LICENSE](LICENSE). The Sirene data is published by INSEE under the
[Licence Ouverte / Etalab 2.0](https://www.etalab.gouv.fr/licence-ouverte-open-licence/).
