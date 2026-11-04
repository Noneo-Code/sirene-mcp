package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/duckdb/duckdb-go/v2"

	"github.com/Noneo-Code/sirene-mcp/internal/nomenclature"
	"github.com/Noneo-Code/sirene-mcp/internal/store"
)

const (
	defaultLimit = 50
	maxLimit     = 500
)

// Establishment is one search hit.
type Establishment struct {
	Siret              string `json:"siret"`
	Siren              string `json:"siren"`
	NomAffiche         string `json:"nom_affiche"`
	NAF                string `json:"naf"`
	NAFLibelle         string `json:"naf_libelle"`
	NAF25              string `json:"naf25"`
	AdresseComplete    string `json:"adresse_complete"`
	CodePostal         string `json:"code_postal"`
	Commune            string `json:"commune"`
	Departement        string `json:"departement"`
	TrancheEffectifs   string `json:"tranche_effectifs"`
	TrancheLibelle     string `json:"tranche_libelle"`
	CategorieJuridique string `json:"categorie_juridique"`
	DateCreation       string `json:"date_creation"`
	Etat               string `json:"etat"`
	EstSiege           bool   `json:"est_siege"`
	DiffusionPartielle bool   `json:"diffusion_partielle"`
}

// establishmentColumns lists the Search result columns, in order, as
// (SQL expression, JSON/CSV name) pairs. Search, GetCompany and Export share it.
var establishmentColumns = [][2]string{
	{"e.siret", "siret"},
	{"e.siren", "siren"},
	{"coalesce(e.nom_affiche,'')", "nom_affiche"},
	{"coalesce(e.activite_principale,'')", "naf"},
	{"coalesce(n.libelle,'')", "naf_libelle"},
	{"coalesce(e.activite_principale_naf25,'')", "naf25"},
	{"coalesce(e.adresse_complete,'')", "adresse_complete"},
	{"coalesce(e.code_postal,'')", "code_postal"},
	{"coalesce(e.libelle_commune,'')", "commune"},
	{"coalesce(e.departement,'')", "departement"},
	{"coalesce(e.tranche_effectifs,'')", "tranche_effectifs"},
	{"coalesce(t.libelle,'')", "tranche_libelle"},
	{"coalesce(e.categorie_juridique,'')", "categorie_juridique"},
	{"coalesce(CAST(e.date_creation AS VARCHAR),'')", "date_creation"},
	{"coalesce(e.etat_administratif,'')", "etat"},
	{"coalesce(e.est_siege,false)", "est_siege"},
	{"coalesce(e.statut_diffusion,'') = 'P'", "diffusion_partielle"},
}

// establishmentSelect selects establishmentColumns over etablissement e and its
// label joins; callers append " WHERE …".
var establishmentSelect = func() string {
	cols := make([]string, len(establishmentColumns))
	for i, c := range establishmentColumns {
		cols[i] = c[0] + " AS " + c[1]
	}
	return "SELECT " + strings.Join(cols, ", ") + `
FROM etablissement e
LEFT JOIN naf n ON n.code = e.activite_principale
LEFT JOIN tranche_effectifs t ON t.code = e.tranche_effectifs`
}()

const establishmentOrder = "ORDER BY e.departement, e.code_postal, e.nom_affiche, e.siret"

// scanEstablishments appends the rows of an establishmentSelect query to dst
// and closes rows.
func scanEstablishments(rows *sql.Rows, dst []Establishment) ([]Establishment, error) {
	defer rows.Close()
	for rows.Next() {
		var e Establishment
		if err := rows.Scan(&e.Siret, &e.Siren, &e.NomAffiche, &e.NAF, &e.NAFLibelle, &e.NAF25, &e.AdresseComplete,
			&e.CodePostal, &e.Commune, &e.Departement, &e.TrancheEffectifs, &e.TrancheLibelle,
			&e.CategorieJuridique, &e.DateCreation, &e.Etat, &e.EstSiege, &e.DiffusionPartielle); err != nil {
			return dst, err
		}
		dst = append(dst, e)
	}
	return dst, rows.Err()
}

// SearchResult is a page of establishments with the real total.
type SearchResult struct {
	Total   int64           `json:"total"`
	Limit   int             `json:"limit"`
	Offset  int             `json:"offset"`
	Results []Establishment `json:"results"`
}

// Search returns establishments matching f. limit defaults to 50 and is capped at 500.
func Search(ctx context.Context, s *store.Store, f Filters, limit, offset int) (SearchResult, error) {
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if offset < 0 {
		offset = 0
	}
	where, args, err := f.Where()
	if err != nil {
		return SearchResult{}, err
	}
	res := SearchResult{Limit: limit, Offset: offset, Results: []Establishment{}}
	err = s.Use(ctx, func(db *sql.DB) error {
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM etablissement e WHERE "+where, args...).Scan(&res.Total); err != nil {
			return err
		}
		q := establishmentSelect + " WHERE " + where + " " + establishmentOrder + " LIMIT ? OFFSET ?" //nolint:gosec // where only holds placeholders
		rows, err := db.QueryContext(ctx, q, append(append([]any{}, args...), limit, offset)...)
		if err != nil {
			return err
		}
		res.Results, err = scanEstablishments(rows, res.Results)
		return err
	})
	if err != nil {
		return SearchResult{}, fmt.Errorf("search: %w", err)
	}
	return res, nil
}

// Bucket is one group of a Count.
type Bucket struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// CountResult is the total and optional breakdown.
type CountResult struct {
	Total   int64    `json:"total"`
	GroupBy string   `json:"group_by"`
	Buckets []Bucket `json:"buckets"`
}

// Count counts establishments matching f, optionally grouped by
// "", departement, naf, tranche_effectifs or categorie_juridique.
func Count(ctx context.Context, s *store.Store, f Filters, groupBy string) (CountResult, error) {
	var col, join string
	switch groupBy {
	case "":
	case "departement":
		col = "e.departement"
	case "naf":
		col, join = "e.activite_principale", "LEFT JOIN naf l ON l.code = e.activite_principale"
	case "tranche_effectifs":
		col, join = "e.tranche_effectifs", "LEFT JOIN tranche_effectifs l ON l.code = e.tranche_effectifs"
	case "categorie_juridique":
		col, join = "e.categorie_juridique", "LEFT JOIN categorie_juridique l ON l.code = e.categorie_juridique"
	default:
		return CountResult{}, fmt.Errorf("group_by: unknown value %q, expected departement, naf, tranche_effectifs or categorie_juridique", groupBy)
	}
	where, args, err := f.Where()
	if err != nil {
		return CountResult{}, err
	}
	res := CountResult{GroupBy: groupBy, Buckets: []Bucket{}}
	err = s.Use(ctx, func(db *sql.DB) error {
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM etablissement e WHERE "+where, args...).Scan(&res.Total); err != nil {
			return err
		}
		if col == "" {
			return nil
		}
		label := "''"
		if join != "" {
			label = "coalesce(any_value(l.libelle),'')"
		}
		//nolint:gosec // G201: col/label/join come from a fixed whitelist, values are bound
		q := fmt.Sprintf(`SELECT coalesce(%[1]s,''), %[2]s, count(*) AS c FROM etablissement e %[3]s WHERE %[4]s
GROUP BY coalesce(%[1]s,'') ORDER BY c DESC, 1`, col, label, join, where)
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b Bucket
			if err := rows.Scan(&b.Key, &b.Label, &b.Count); err != nil {
				return err
			}
			res.Buckets = append(res.Buckets, b)
		}
		return rows.Err()
	})
	if err != nil {
		return CountResult{}, fmt.Errorf("count: %w", err)
	}
	return res, nil
}

// Company is a legal unit with its head office and a page of its establishments.
type Company struct {
	Siren                     string          `json:"siren"`
	StatutDiffusion           string          `json:"statut_diffusion"`
	Denomination              string          `json:"denomination"`
	Sigle                     string          `json:"sigle"`
	Nom                       string          `json:"nom"`
	NomUsage                  string          `json:"nom_usage"`
	Prenom                    string          `json:"prenom"`
	CategorieJuridique        string          `json:"categorie_juridique"`
	CategorieJuridiqueLibelle string          `json:"categorie_juridique_libelle"`
	NAF                       string          `json:"naf"`
	NAFLibelle                string          `json:"naf_libelle"`
	NAF25                     string          `json:"naf25"`
	Etat                      string          `json:"etat"`
	CategorieEntreprise       string          `json:"categorie_entreprise"`
	TrancheEffectifs          string          `json:"tranche_effectifs"`
	TrancheLibelle            string          `json:"tranche_libelle"`
	EconomieSocialeSolidaire  string          `json:"economie_sociale_solidaire"`
	NicSiege                  string          `json:"nic_siege"`
	DateCreation              string          `json:"date_creation"`
	DateDernierTraitement     string          `json:"date_dernier_traitement"`
	DiffusionPartielle        bool            `json:"diffusion_partielle"`
	Siege                     *Establishment  `json:"siege"`
	Establishments            []Establishment `json:"establishments"`
	TotalEstablishments       int64           `json:"total_establishments"`
	Limit                     int             `json:"limit"`
	Offset                    int             `json:"offset"`
}

// GetCompany looks up a legal unit by SIREN (9 digits) or SIRET (14 digits,
// resolved to its SIREN); spaces are ignored. Establishments of every state are
// listed, head office first, limit defaulting to 50 and capped at 500.
func GetCompany(ctx context.Context, s *store.Store, sirenOrSiret string, limit, offset int) (Company, error) {
	id := strings.Join(strings.Fields(sirenOrSiret), "")
	if strings.Trim(id, "0123456789") != "" || (len(id) != 9 && len(id) != 14) {
		return Company{}, fmt.Errorf("company: %q is neither a SIREN (9 digits) nor a SIRET (14 digits)", sirenOrSiret)
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if offset < 0 {
		offset = 0
	}
	c := Company{Limit: limit, Offset: offset, Establishments: []Establishment{}}
	err := s.Use(ctx, func(db *sql.DB) error {
		siren := id
		if len(id) == 14 {
			err := db.QueryRowContext(ctx, "SELECT siren FROM etablissement WHERE siret = ?", id).Scan(&siren)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("SIRET %s not found", id)
			}
			if err != nil {
				return err
			}
		}
		err := db.QueryRowContext(ctx, `SELECT u.siren, coalesce(u.statut_diffusion,''), coalesce(u.denomination,''),
  coalesce(u.sigle,''), coalesce(u.nom,''), coalesce(u.nom_usage,''), coalesce(u.prenom,''),
  coalesce(u.categorie_juridique,''), coalesce(cj.libelle,''), coalesce(u.activite_principale,''), coalesce(n.libelle,''),
  coalesce(u.activite_principale_naf25,''), coalesce(u.etat_administratif,''), coalesce(u.categorie_entreprise,''),
  coalesce(u.tranche_effectifs,''), coalesce(t.libelle,''), coalesce(u.economie_sociale_solidaire,''),
  coalesce(u.nic_siege,''), coalesce(CAST(u.date_creation AS VARCHAR),''),
  coalesce(CAST(u.date_dernier_traitement AS VARCHAR),''), coalesce(u.statut_diffusion,'') = 'P'
FROM unite_legale u
LEFT JOIN categorie_juridique cj ON cj.code = u.categorie_juridique
LEFT JOIN naf n ON n.code = u.activite_principale
LEFT JOIN tranche_effectifs t ON t.code = u.tranche_effectifs
WHERE u.siren = ?`, siren).Scan(&c.Siren, &c.StatutDiffusion, &c.Denomination, &c.Sigle, &c.Nom, &c.NomUsage,
			&c.Prenom, &c.CategorieJuridique, &c.CategorieJuridiqueLibelle, &c.NAF, &c.NAFLibelle, &c.NAF25, &c.Etat,
			&c.CategorieEntreprise, &c.TrancheEffectifs, &c.TrancheLibelle, &c.EconomieSocialeSolidaire, &c.NicSiege,
			&c.DateCreation, &c.DateDernierTraitement, &c.DiffusionPartielle)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("SIREN %s not found", siren)
		}
		if err != nil {
			return err
		}
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM etablissement WHERE siren = ?", siren).Scan(&c.TotalEstablishments); err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, establishmentSelect+" WHERE e.siren = ? AND e.siret = e.siren || ?", siren, c.NicSiege)
		if err != nil {
			return err
		}
		siege, err := scanEstablishments(rows, nil)
		if err != nil {
			return err
		}
		if len(siege) > 0 {
			c.Siege = &siege[0]
		}
		rows, err = db.QueryContext(ctx, establishmentSelect+` WHERE e.siren = ?
ORDER BY e.siret = e.siren || ? DESC, e.etat_administratif, e.siret LIMIT ? OFFSET ?`, siren, c.NicSiege, limit, offset)
		if err != nil {
			return err
		}
		c.Establishments, err = scanEstablishments(rows, c.Establishments)
		return err
	})
	if err != nil {
		return Company{}, fmt.Errorf("company: %w", err)
	}
	return c, nil
}

var nomenclatureTables = map[string]bool{"naf": true, "naf2025": true, "categorie_juridique": true, "tranche_effectifs": true}

// foldText lowercases s, strips French accents, unifies apostrophes (NAF 2025
// uses ’, NAF rév. 2 uses ') and collapses spaces, so both sides of a
// nomenclature match compare equal.
func foldText(s string) string {
	s = strings.NewReplacer("’", "'", "‘", "'", "ʼ", "'", "`", "'", "´", "'").Replace(s)
	return strings.Join(strings.Fields(normalizeName(s)), " ")
}

// foldCode removes dots and spaces and uppercases a nomenclature code.
func foldCode(s string) string {
	return strings.ToUpper(strings.NewReplacer(".", "", " ", "").Replace(s))
}

// SearchNomenclature returns the entries of table (naf, naf2025,
// categorie_juridique or tranche_effectifs) whose label contains every word of
// text, whose code starts with text, or (NAF tables) whose curated alias
// matches text (every word of text starts a word of the alias, e.g. "kiné"
// finds 86.90E through "kinésithérapeute"). Case, accents, apostrophe style
// and dots in codes are ignored. Entries keep their official code and label.
// Empty text lists the whole table. limit <= 0 means no limit: the tables are
// small (under 800 rows) and the tools must not truncate silently.
func SearchNomenclature(ctx context.Context, s *store.Store, table, text string, limit int) ([]nomenclature.Entry, error) {
	if !nomenclatureTables[table] {
		return nil, fmt.Errorf("nomenclature: unknown table %q, expected naf, naf2025, categorie_juridique or tranche_effectifs", table)
	}
	words := strings.Fields(foldText(text))
	code := foldCode(text)
	aliased := aliasCodes(table, text)
	out := []nomenclature.Entry{}
	err := s.Use(ctx, func(db *sql.DB) error {
		rows, err := db.QueryContext(ctx, "SELECT code, coalesce(libelle,'') FROM "+table+" ORDER BY code") //nolint:gosec // table is checked against nomenclatureTables
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() && (limit <= 0 || len(out) < limit) {
			var e nomenclature.Entry
			if err := rows.Scan(&e.Code, &e.Label); err != nil {
				return err
			}
			if aliased[e.Code] || matchEntry(e, words, code) {
				out = append(out, e)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("nomenclature: %w", err)
	}
	return out, nil
}

// aliasWords folds s like foldText and also splits on hyphens, so
// "masseur-kinésithérapeute" and "expert comptable" compare word by word.
func aliasWords(s string) []string {
	return strings.Fields(strings.ReplaceAll(foldText(s), "-", " "))
}

// aliasCodes returns the codes of table whose curated alias matches text:
// every word of text is a prefix of some word of the alias.
func aliasCodes(table, text string) map[string]bool {
	words := aliasWords(text)
	if len(words) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, a := range nomenclature.Aliases()[table] {
		aw := aliasWords(a.Alias)
		if allPrefixOfSome(words, aw) {
			out[a.Code] = true
		}
	}
	return out
}

func allPrefixOfSome(words, of []string) bool {
	for _, w := range words {
		found := false
		for _, o := range of {
			if strings.HasPrefix(o, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func matchEntry(e nomenclature.Entry, words []string, code string) bool {
	if len(words) == 0 || (code != "" && strings.HasPrefix(foldCode(e.Code), code)) {
		return true
	}
	label := foldText(e.Label)
	for _, w := range words {
		if !strings.Contains(label, w) {
			return false
		}
	}
	return true
}

// Info describes the loaded database.
type Info struct {
	LastSync                 time.Time `json:"last_sync"`
	ExcludeClosed            bool      `json:"exclude_closed"`
	ULRows                   int64     `json:"ul_rows"`
	EtabRows                 int64     `json:"etab_rows"`
	ULSource                 string    `json:"ul_source"`
	EtabSource               string    `json:"etab_source"`
	ULLastModified           string    `json:"ul_last_modified"`
	EtabLastModified         string    `json:"etab_last_modified"`
	MaxDateDernierTraitement string    `json:"max_date_dernier_traitement"`
}

// GetInfo reads the latest completed sync_log row (by finished_at) and the
// freshest record date.
func GetInfo(ctx context.Context, s *store.Store) (Info, error) {
	var i Info
	err := s.Use(ctx, func(db *sql.DB) error {
		err := db.QueryRowContext(ctx, `SELECT finished_at, coalesce(exclude_closed,false), coalesce(ul_rows,0),
  coalesce(etab_rows,0), coalesce(ul_source,''), coalesce(etab_source,''), coalesce(ul_last_modified,''),
  coalesce(etab_last_modified,'')
FROM sync_log WHERE finished_at IS NOT NULL ORDER BY finished_at DESC LIMIT 1`).Scan(&i.LastSync, &i.ExcludeClosed, &i.ULRows,
			&i.EtabRows, &i.ULSource, &i.EtabSource, &i.ULLastModified, &i.EtabLastModified)
		if errors.Is(err, sql.ErrNoRows) {
			// Rows with a NULL finished_at are unfinished syncs: ignored.
			return errors.New("sync_log has no completed sync (finished_at is empty)")
		}
		if err != nil {
			return err
		}
		return db.QueryRowContext(ctx, `SELECT coalesce(CAST(greatest(
  (SELECT max(date_dernier_traitement) FROM unite_legale),
  (SELECT max(date_dernier_traitement) FROM etablissement)) AS VARCHAR), '')`).Scan(&i.MaxDateDernierTraitement)
	})
	if err != nil {
		return Info{}, fmt.Errorf("info: %w", err)
	}
	return i, nil
}

// ExportResult describes a written export file.
type ExportResult struct {
	Path   string `json:"path"`
	Rows   int64  `json:"rows"`
	Format string `json:"format"`
}

// Export writes every establishment matching f, with the Search columns and
// order, to a new file <dir>/sirene_<YYYYMMDD-HHMMSS>[_N].<csv|json> (a JSON
// array for json). dir is created if missing; existing files are never
// overwritten. It runs on the main pool, the only one allowed to write files.
func Export(ctx context.Context, s *store.Store, f Filters, format, dir string) (ExportResult, error) {
	var opts string
	switch format {
	case "csv":
		opts = "FORMAT csv, HEADER true"
	case "json":
		opts = "FORMAT json, ARRAY true"
	default:
		return ExportResult{}, fmt.Errorf("export: unknown format %q, expected csv or json", format)
	}
	where, args, err := f.Where()
	if err != nil {
		return ExportResult{}, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return ExportResult{}, fmt.Errorf("export: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ExportResult{}, fmt.Errorf("export: %w", err)
	}
	path, err := reserveFile(dir, "sirene_"+time.Now().Format("20060102-150405"), format)
	if err != nil {
		return ExportResult{}, fmt.Errorf("export: %w", err)
	}
	res := ExportResult{Path: path, Format: format}
	// where only holds placeholders; path is built here and quoted.
	q := fmt.Sprintf("COPY (%s WHERE %s %s) TO %s (%s)", establishmentSelect, where, establishmentOrder, quote(path), opts) //nolint:gosec
	err = s.Use(ctx, func(db *sql.DB) error {
		r, err := db.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		res.Rows, err = r.RowsAffected()
		return err
	})
	if err != nil {
		_ = os.Remove(path)
		return ExportResult{}, fmt.Errorf("export: %w", err)
	}
	return res, nil
}

// reserveFile creates an empty, new file dir/base.ext (or dir/base_N.ext if
// taken) and returns its path; COPY then fills it.
func reserveFile(dir, base, ext string) (string, error) {
	for n := 0; n < 1000; n++ {
		name := base + "." + ext
		if n > 0 {
			name = fmt.Sprintf("%s_%d.%s", base, n, ext)
		}
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return p, f.Close()
	}
	return "", fmt.Errorf("no free file name for %s in %s", base, dir)
}

func quote(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

const (
	defaultSQLRows    = 1000
	maxSQLRows        = 10000
	defaultSQLTimeout = 30 * time.Second
)

// SQLResult is the outcome of RunSQL.
type SQLResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
}

// RunSQL runs one read-only SELECT written by the caller (an LLM) on the locked
// pool, which has no file or network access and a locked configuration.
// maxRows defaults to 1000 and is capped at 10000; timeout defaults to 30s.
//
// Validation, before anything runs (see checkSelect): the first keyword must
// be SELECT or WITH (after comments and opening parentheses), and DuckDB's own
// parser, through json_serialize_sql, must find exactly one statement whose
// whole tree, CTEs and subqueries included, only holds read-only query nodes
// (SELECT_NODE, SET_OPERATION_NODE, RECURSIVE_CTE_NODE, CTE_NODE) and no
// RETURNING clause. json_serialize_sql only serializes SELECT statements:
// top-level DML, DDL, COPY, ATTACH, SET, PRAGMA, CALL, EXPLAIN… come back as
// {"error": true}, while data-modifying CTEs (WITH a AS (DELETE … RETURNING *)
// SELECT …) serialize as DELETE/UPDATE/INSERT_QUERY_NODE and are caught by the
// tree walk. The READ_ONLY attach of the file is a second layer, and table
// functions reading files are stopped at execution by
// enable_external_access=false.
//
// The validated query then runs wrapped in SELECT * FROM (…) LIMIT maxRows+1
// (see boundQuery), so DuckDB never builds more rows than the page needs.
func RunSQL(ctx context.Context, s *store.Store, query string, maxRows int, timeout time.Duration) (SQLResult, error) {
	if maxRows <= 0 {
		maxRows = defaultSQLRows
	}
	if maxRows > maxSQLRows {
		maxRows = maxSQLRows
	}
	if timeout <= 0 {
		timeout = defaultSQLTimeout
	}
	if kw := firstKeyword(query); kw != "SELECT" && kw != "WITH" {
		return SQLResult{}, errors.New("sql: only a single SELECT (or WITH … SELECT) query is allowed")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res := SQLResult{Rows: [][]any{}}
	err := s.UseLocked(ctx, func(db *sql.DB) error {
		if err := checkSelect(ctx, db, query); err != nil {
			return err
		}
		bounded, err := boundQuery(ctx, db, query, maxRows+1)
		if err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, bounded)
		if err != nil {
			return err
		}
		defer rows.Close()
		if res.Columns, err = rows.Columns(); err != nil {
			return err
		}
		for rows.Next() {
			if len(res.Rows) == maxRows {
				res.Truncated = true
				break
			}
			vals := make([]any, len(res.Columns))
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			for i, v := range vals {
				vals[i] = jsonValue(v)
			}
			res.Rows = append(res.Rows, vals)
		}
		return rows.Err()
	})
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return SQLResult{}, fmt.Errorf("sql: timed out after %s", timeout)
		}
		return SQLResult{}, fmt.Errorf("sql: %w", err)
	}
	return res, nil
}

// boundQuery wraps the validated query so that DuckDB itself stops after limit
// rows: the driver materializes a whole result before QueryContext returns, so
// a bound applied while scanning would not stop SELECT * FROM etablissement
// from building tens of millions of rows. The extra row past maxRows tells
// RunSQL the result was truncated. DuckDB keeps the inner ORDER BY under the
// outer LIMIT (it plans the pair as a top-N).
//
// The query goes in verbatim, after its trailing semicolons and whitespace,
// on lines of its own so a trailing -- comment cannot swallow the closing
// parenthesis. If that text does not wrap into a single read-only statement
// (say "SELECT 1; -- done"), DuckDB's own rendering of the parsed statement
// (json_deserialize_sql) is wrapped instead. Either way the wrapped text goes
// through checkSelect again before it runs.
func boundQuery(ctx context.Context, db *sql.DB, query string, limit int) (string, error) {
	wrap := func(q string) string {
		q = strings.TrimRight(q, " \t\r\n\f;")
		return fmt.Sprintf("SELECT * FROM (\n%s\n) AS q LIMIT %d", q, limit)
	}
	w := wrap(query)
	if checkSelect(ctx, db, w) == nil {
		return w, nil
	}
	var canon string
	if err := db.QueryRowContext(ctx, "SELECT json_deserialize_sql(json_serialize_sql(?))", query).Scan(&canon); err != nil {
		return "", err
	}
	w = wrap(canon)
	if err := checkSelect(ctx, db, w); err != nil {
		return "", err
	}
	return w, nil
}

// checkSelect parses query with DuckDB and requires exactly one SELECT statement.
func checkSelect(ctx context.Context, db *sql.DB, query string) error {
	var raw string
	if err := db.QueryRowContext(ctx, "SELECT CAST(json_serialize_sql(?) AS VARCHAR)", query).Scan(&raw); err != nil {
		return err
	}
	var parsed struct {
		Error        bool   `json:"error"`
		ErrorType    string `json:"error_type"`
		ErrorMessage string `json:"error_message"`
		Statements   []struct {
			Node struct {
				Type string `json:"type"`
			} `json:"node"`
		} `json:"statements"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return fmt.Errorf("cannot parse query: %w", err)
	}
	if parsed.Error {
		if parsed.ErrorType == "not implemented" {
			return errors.New("only a single SELECT (or WITH … SELECT) query is allowed")
		}
		return fmt.Errorf("invalid query (%s): %s", parsed.ErrorType, parsed.ErrorMessage)
	}
	if len(parsed.Statements) != 1 {
		return fmt.Errorf("exactly one statement is allowed, got %d", len(parsed.Statements))
	}
	var tree any
	if err := json.Unmarshal([]byte(raw), &tree); err != nil {
		return fmt.Errorf("cannot parse query: %w", err)
	}
	return checkTree(tree)
}

// readOnlyNodes are the query node types a read-only query may contain.
var readOnlyNodes = map[string]bool{
	"SELECT_NODE": true, "SET_OPERATION_NODE": true, "RECURSIVE_CTE_NODE": true, "CTE_NODE": true,
}

// checkTree walks a json_serialize_sql tree and rejects any query node that is
// not a read-only one (DELETE_QUERY_NODE, UPDATE_QUERY_NODE,
// INSERT_QUERY_NODE, …), wherever it sits, and any RETURNING clause.
func checkTree(v any) error {
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["returning_list"]; ok {
			return errors.New("only read-only SELECT queries are allowed (data-modifying clause found)")
		}
		if t, ok := x["type"].(string); ok && strings.HasSuffix(t, "_NODE") && !readOnlyNodes[t] {
			return fmt.Errorf("only read-only SELECT queries are allowed, found %s", t)
		}
		for _, e := range x {
			if err := checkTree(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := checkTree(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// firstKeyword returns the first word of query, uppercased, skipping
// whitespace, -- and /* */ comments and opening parentheses.
func firstKeyword(q string) string {
	for {
		q = strings.TrimLeft(q, " \t\r\n\f(")
		switch {
		case strings.HasPrefix(q, "--"):
			i := strings.IndexByte(q, '\n')
			if i < 0 {
				return ""
			}
			q = q[i+1:]
		case strings.HasPrefix(q, "/*"):
			i := strings.Index(q, "*/")
			if i < 0 {
				return ""
			}
			q = q[i+2:]
		default:
			end := strings.IndexFunc(q, func(r rune) bool {
				return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
			})
			if end < 0 {
				end = len(q)
			}
			return strings.ToUpper(q[:end])
		}
	}
}

// jsonValue converts a scanned DuckDB value to something encoding/json renders
// faithfully: times as RFC 3339 strings, bytes as strings, decimals as floats,
// UUIDs as strings, maps with string keys.
func jsonValue(v any) any {
	switch x := v.(type) {
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case []byte:
		return string(x)
	case duckdb.Decimal:
		return x.Float64()
	case duckdb.UUID:
		return x.String()
	case *duckdb.UUID:
		if x == nil {
			return nil
		}
		return x.String()
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = jsonValue(e)
		}
		return out
	case duckdb.Map:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[fmt.Sprint(jsonValue(k))] = jsonValue(e)
		}
		return out
	default:
		return v
	}
}
