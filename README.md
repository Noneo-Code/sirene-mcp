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

## Install

Download the archive for your platform from the
[releases page](https://github.com/Noneo-Code/sirene-mcp/releases) (linux amd64/arm64, macOS arm64),
check it against `checksums.txt`, and put the binary on your `PATH`:

```bash
tar -xzf sirene-mcp_darwin_arm64.tar.gz
mv sirene-mcp ~/.local/bin/
sirene-mcp version
```

Or build from source (needs Go and a C toolchain, because DuckDB is linked through CGO):

```bash
CGO_ENABLED=1 go install github.com/Noneo-Code/sirene-mcp/cmd/sirene-mcp@latest
```

## Quick start

```bash
sirene-mcp sync                       # download the stock files and build the database (once a month)
sirene-mcp status                     # database location, row counts, last sync
claude mcp add sirene -- sirene-mcp stdio
```

### Sync

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

Requirements:

| | Full sync | `--exclude-closed` |
|---|---|---|
| Download | ~0.7 GB + ~2.2 GB Parquet <!-- measured in Task 12 --> | <!-- measured in Task 12 --> |
| Disk (database) | <!-- measured in Task 12 --> | <!-- measured in Task 12 --> |
| Peak disk during a sync | <!-- measured in Task 12 --> (old and new database coexist, plus the downloads) | <!-- measured in Task 12 --> |
| RAM | <!-- measured in Task 12 --> (default `--memory-limit 4GB`) | <!-- measured in Task 12 --> |
| Duration | <!-- measured in Task 12 --> | <!-- measured in Task 12 --> |

Source URLs can be overridden with `--ul-url` / `--etab-url` (or `SIRENE_MCP_UL_URL` /
`SIRENE_MCP_ETAB_URL`), for instance to point at a mirror.

### Data directory

By default everything lives in `<user cache dir>/sirene-mcp` (`~/Library/Caches/sirene-mcp` on macOS,
`~/.cache/sirene-mcp` on Linux). Override it with `--data-dir DIR` or `SIRENE_MCP_DATA_DIR`
(the flag goes after the command). Exports are written to `<data dir>/exports` unless you pass
`--export-dir`.

## Connect a client

### stdio (local, recommended)

```bash
claude mcp add sirene -- sirene-mcp stdio
# a custom data directory:
claude mcp add sirene --env SIRENE_MCP_DATA_DIR=/data/sirene -- sirene-mcp stdio
claude mcp list   # sirene: ... Connected
```

Add `--scope user` to make it available in every project. Any MCP client that can launch a
command works the same way: run `sirene-mcp stdio`.

### Streamable HTTP

For a client on another machine or a long-running shared server:

```bash
sirene-mcp serve                      # listens on 127.0.0.1:8766, MCP endpoint at /mcp
sirene-mcp serve --auto-sync          # also refreshes the database monthly (accepts the sync flags)

claude mcp add --transport http sirene http://127.0.0.1:8766/mcp \
  --header "Authorization: Bearer $(sirene-mcp token)"
```

`sirene-mcp token` prints the bearer token (generated on first use and stored in the data
directory; `SIRENE_MCP_TOKEN` overrides it). The server refuses non-loopback addresses unless you
pass `--allow-remote`; in that case put it behind a TLS reverse proxy.

## Tools

| Tool | What it does |
|---|---|
| `search_establishments` | List establishments matching filters (name, SIREN, NAF, department, size, legal category, head office only, active or closed...), paginated with a real `total` |
| `count_establishments` | Count matches, optionally grouped by department, NAF code, headcount band or legal category |
| `get_company` | One legal unit by SIREN or SIRET: identity, head office and its establishments |
| `search_naf` | Find NAF codes by label or prefix, in NAF rev. 2 or NAF 2025 |
| `search_legal_categories` | Find legal category codes (SAS, SARL, association...) |
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
- Binaries are built with CGO (DuckDB): linux amd64/arm64 and macOS arm64 only.

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
