// Package mcpserver exposes the Sirene database as Model Context Protocol tools.
package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Noneo-Code/sirene-mcp/internal/nomenclature"
	"github.com/Noneo-Code/sirene-mcp/internal/query"
	"github.com/Noneo-Code/sirene-mcp/internal/store"
)

const instructions = "Query a local copy of the French INSEE Sirene register (all companies and establishments). " +
	"Use search_naf / search_legal_categories to find codes instead of guessing them, " +
	"count_establishments to size a query before listing, and export_establishments for full lists. " +
	"Results only contain what Sirene publishes: never invent missing data. " +
	"Rows with diffusion_partielle=true belong to individuals who restricted publication; masked fields read [ND]. " +
	"Mention the data date from data_info when reporting results."

type server struct {
	store     *store.Store
	exportDir string
}

// FilterIn is the filter part shared by the establishment tools.
type FilterIn struct {
	NAF                  []string `json:"naf,omitempty" jsonschema:"NAF rev. 2 codes or prefixes like 86.90E or 86.9 (dots optional); use search_naf to find codes"`
	NAF25                []string `json:"naf25,omitempty" jsonschema:"NAF 2025 codes or prefixes like 86.90F or 86.9; use search_naf with version 2025 to find codes"`
	Departements         []string `json:"departements,omitempty" jsonschema:"department codes: 2 digits (30), 2A/2B for Corsica, 3 digits for overseas (971)"`
	Communes             []string `json:"communes,omitempty" jsonschema:"5-character INSEE commune codes like 30189 (not postal codes)"`
	CodesPostaux         []string `json:"codes_postaux,omitempty" jsonschema:"5-digit postal codes like 30000"`
	Tranches             []string `json:"tranches_effectifs,omitempty" jsonschema:"headcount band codes like 00, 01, 02, 03, 11, 12, 21, 22, 31, 32, 41, 42, 51, 52, 53; the tranche_effectifs table lists them (sql_query)"`
	CategoriesJuridiques []string `json:"categories_juridiques,omitempty" jsonschema:"legal category codes (4 digits, e.g. 5710 for SAS) or prefixes (e.g. 57); use search_legal_categories to find codes"`
	CreatedAfter         string   `json:"created_after,omitempty" jsonschema:"creation date lower bound, inclusive, format YYYY-MM-DD"`
	CreatedBefore        string   `json:"created_before,omitempty" jsonschema:"creation date upper bound, inclusive, format YYYY-MM-DD"`
	SiegeOnly            bool     `json:"siege_only,omitempty" jsonschema:"only head-office establishments"`
	Etat                 string   `json:"etat,omitempty" jsonschema:"administrative state: A (active, the default), F (closed) or all"`
	Name                 string   `json:"name,omitempty" jsonschema:"text to find in the name, trade name, acronym or legal name (accents and case ignored)"`
}

func (f FilterIn) filters() query.Filters {
	return query.Filters{
		NAF: f.NAF, NAF25: f.NAF25, Departements: f.Departements, Communes: f.Communes,
		CodesPostaux: f.CodesPostaux, Tranches: f.Tranches, CategoriesJuridiques: f.CategoriesJuridiques,
		CreatedAfter: f.CreatedAfter, CreatedBefore: f.CreatedBefore,
		SiegeOnly: f.SiegeOnly, Etat: f.Etat, Name: f.Name,
	}
}

type searchIn struct {
	FilterIn
	Limit  int `json:"limit,omitempty" jsonschema:"page size, default 50, max 500"`
	Offset int `json:"offset,omitempty" jsonschema:"number of results to skip, for pagination"`
}

type countIn struct {
	FilterIn
	GroupBy string `json:"group_by,omitempty" jsonschema:"optional breakdown: departement, naf, tranche_effectifs or categorie_juridique"`
}

type companyIn struct {
	SirenOrSiret string `json:"siren_or_siret" jsonschema:"SIREN (9 digits) or SIRET (14 digits); spaces are ignored"`
	Limit        int    `json:"limit,omitempty" jsonschema:"establishments page size, default 50, max 500"`
	Offset       int    `json:"offset,omitempty" jsonschema:"establishments to skip, for pagination"`
}

type nafIn struct {
	Text    string `json:"text" jsonschema:"words from the activity label, a profession or business word (kiné, plombier...), or a code or code prefix; empty lists the whole table"`
	Version string `json:"version,omitempty" jsonschema:"rev2 (default, NAF rev. 2) or 2025 (NAF 2025)"`
}

type legalIn struct {
	Text string `json:"text" jsonschema:"words from the legal category label, or a code or code prefix; empty lists the whole table"`
}

type exportIn struct {
	FilterIn
	Format string `json:"format,omitempty" jsonschema:"csv (default) or json"`
}

type sqlIn struct {
	Query   string `json:"query" jsonschema:"one SELECT or WITH statement"`
	MaxRows int    `json:"max_rows,omitempty" jsonschema:"maximum rows returned, default 1000, capped at 10000"`
}

type emptyIn struct{}

// EntriesOut is the complete list of matching nomenclature entries (never
// truncated); Total is its length.
type EntriesOut struct {
	Total   int                  `json:"total"`
	Entries []nomenclature.Entry `json:"entries"`
}

func entriesOut(es []nomenclature.Entry) EntriesOut {
	es = nonNil(es)
	return EntriesOut{Total: len(es), Entries: es}
}

// New builds the MCP server over s; exports are written under exportDir.
func New(s *store.Store, exportDir, version string) *mcp.Server {
	h := &server{store: s, exportDir: exportDir}
	srv := mcp.NewServer(&mcp.Implementation{Name: "sirene-mcp", Title: "Sirene", Version: version}, &mcp.ServerOptions{
		Instructions: instructions,
	})
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	writesFile := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}

	mcp.AddTool(srv, &mcp.Tool{Name: "search_establishments", Annotations: readOnly,
		Description: "List active establishments (closed ones with etat=F or all) matching the filters, in a stable order. " +
			"Results are paginated (limit, offset) and `total` is the real number of matches, not the page size. " +
			"Rows with diffusion_partielle=true have masked fields ([ND])."}, h.search)
	mcp.AddTool(srv, &mcp.Tool{Name: "count_establishments", Annotations: readOnly,
		Description: "Count establishments matching the filters, optionally broken down by departement, naf, tranche_effectifs or categorie_juridique. Use it to size a query before listing."}, h.count)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_company", Annotations: readOnly,
		Description: "Look up one legal unit by SIREN or SIRET: identity, head office and a page of all its establishments."}, h.company)
	mcp.AddTool(srv, &mcp.Tool{Name: "search_naf", Annotations: readOnly,
		Description: "Find NAF activity codes by label words, common profession words or code prefix, in NAF rev. 2 (default) or NAF 2025. " +
			"Returns every match (total = number of entries). Labels are the official INSEE wording; " +
			"profession words such as kiné or plombier are matched through a small curated alias list, which is only a hint: " +
			"check the returned official label fits, and search related words when unsure."}, h.naf)
	mcp.AddTool(srv, &mcp.Tool{Name: "search_legal_categories", Annotations: readOnly,
		Description: "Find legal category codes (catégories juridiques: SAS, SARL, association...) by label words or code prefix. Returns every match (total = number of entries)."}, h.legal)
	mcp.AddTool(srv, &mcp.Tool{Name: "export_establishments", Annotations: writesFile,
		Description: "Write every establishment matching the filters to a CSV or JSON file on the server and return its path, row count and format. " +
			"Use it for full lists instead of paging through search_establishments."}, h.export)
	mcp.AddTool(srv, &mcp.Tool{Name: "sql_query", Annotations: readOnly,
		Description: "Run a custom SQL query (DuckDB dialect). A single SELECT/WITH statement only, read-only, no file access. " +
			"max_rows defaults to 1000 and is capped at 10000. " +
			"Non-finite floats come back as the strings \"NaN\", \"Infinity\" and \"-Infinity\". " +
			"Tables: unite_legale, etablissement, naf, naf2025, categorie_juridique, tranche_effectifs, sync_log. " +
			"Prefer the dedicated tools (search_establishments, count_establishments, get_company...) when they fit."}, h.sql)
	mcp.AddTool(srv, &mcp.Tool{Name: "data_info", Annotations: readOnly,
		Description: "Describe the loaded database: last sync date, row counts, sources and the freshest record date. Mention it when reporting results."}, h.info)
	return srv
}

func (h *server) search(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, query.SearchResult, error) {
	out, err := query.Search(ctx, h.store, in.filters(), in.Limit, in.Offset)
	return nil, out, err
}

func (h *server) count(ctx context.Context, _ *mcp.CallToolRequest, in countIn) (*mcp.CallToolResult, query.CountResult, error) {
	out, err := query.Count(ctx, h.store, in.filters(), in.GroupBy)
	return nil, out, err
}

func (h *server) company(ctx context.Context, _ *mcp.CallToolRequest, in companyIn) (*mcp.CallToolResult, query.Company, error) {
	out, err := query.GetCompany(ctx, h.store, in.SirenOrSiret, in.Limit, in.Offset)
	return nil, out, err
}

func (h *server) naf(ctx context.Context, _ *mcp.CallToolRequest, in nafIn) (*mcp.CallToolResult, EntriesOut, error) {
	var table string
	switch in.Version {
	case "", "rev2":
		table = "naf"
	case "2025":
		table = "naf2025"
	default:
		return nil, EntriesOut{}, fmt.Errorf("version must be %q or %q, got %q", "rev2", "2025", in.Version)
	}
	entries, err := query.SearchNomenclature(ctx, h.store, table, in.Text, 0)
	return nil, entriesOut(entries), err
}

func (h *server) legal(ctx context.Context, _ *mcp.CallToolRequest, in legalIn) (*mcp.CallToolResult, EntriesOut, error) {
	entries, err := query.SearchNomenclature(ctx, h.store, "categorie_juridique", in.Text, 0)
	return nil, entriesOut(entries), err
}

func (h *server) export(ctx context.Context, _ *mcp.CallToolRequest, in exportIn) (*mcp.CallToolResult, query.ExportResult, error) {
	format := in.Format
	if format == "" {
		format = "csv"
	}
	out, err := query.Export(ctx, h.store, in.filters(), format, h.exportDir)
	return nil, out, err
}

func (h *server) sql(ctx context.Context, _ *mcp.CallToolRequest, in sqlIn) (*mcp.CallToolResult, query.SQLResult, error) {
	out, err := query.RunSQL(ctx, h.store, in.Query, in.MaxRows, 30*time.Second)
	return nil, out, err
}

func (h *server) info(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, query.Info, error) {
	out, err := query.GetInfo(ctx, h.store)
	return nil, out, err
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func ptr[T any](v T) *T { return &v }
