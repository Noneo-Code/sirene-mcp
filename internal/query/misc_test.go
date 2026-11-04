package query

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Noneo-Code/sirene-mcp/internal/nomenclature"
	"github.com/Noneo-Code/sirene-mcp/internal/sirenetest"
	"github.com/Noneo-Code/sirene-mcp/internal/store"
	"github.com/Noneo-Code/sirene-mcp/internal/syncer"
)

func TestGetCompanyBySirenAndSiret(t *testing.T) {
	s := testStore(t)
	c, err := GetCompany(context.Background(), s, "111 111 111", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.Siren != "111111111" || c.TotalEstablishments != 2 || len(c.Establishments) != 2 {
		t.Fatalf("got %+v", c)
	}
	if c.Siege == nil || c.Siege.Siret != "11111111100011" {
		t.Fatalf("siege = %+v", c.Siege)
	}
	if c.Denomination != "Kiné Sud" || c.NAFLibelle == "" || c.CategorieJuridiqueLibelle == "" || c.DiffusionPartielle {
		t.Errorf("incomplete company %+v", c)
	}
	c2, err := GetCompany(context.Background(), s, "11111111100029", 0, 0)
	if err != nil || c2.Siren != "111111111" || c2.TotalEstablishments != 2 {
		t.Fatalf("by siret: %+v err=%v", c2, err)
	}
	if p, _ := GetCompany(context.Background(), s, "222222222", 0, 0); !p.DiffusionPartielle || p.Nom != "[ND]" {
		t.Errorf("partial company %+v", p)
	}
}

func TestGetCompanyBadInput(t *testing.T) {
	s := testStore(t)
	for _, in := range []string{"123", "abcdefghi", "1111111110001"} {
		if _, err := GetCompany(context.Background(), s, in, 0, 0); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
	for _, in := range []string{"999999999", "99999999900011"} {
		if _, err := GetCompany(context.Background(), s, in, 0, 0); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%q: want not found, got %v", in, err)
		}
	}
}

func TestSearchNomenclatureKine(t *testing.T) {
	s := testStore(t)
	has := func(es []nomenclature.Entry, code string) bool {
		for _, e := range es {
			if e.Code == code {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct{ table, text, code string }{
		{"naf", "reeducation", "86.90E"},
		{"naf", "RÉÉDUCATION", "86.90E"},
		{"naf", "l'appareillage", "86.90E"},
		{"naf", "8690e", "86.90E"},
		{"naf2025", "l'exception du riz", "01.11Y"},
		{"naf2025", "l’exception du riz", "01.11Y"},
		{"categorie_juridique", "entrepreneur individuel", "1000"}, //nolint:misspell // French
		{"tranche_effectifs", "non employeuse", "NN"},
	} {
		es, err := SearchNomenclature(context.Background(), s, tc.table, tc.text, 0)
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if !has(es, tc.code) {
			t.Errorf("%+v: %v", tc, es)
		}
	}
	if _, err := SearchNomenclature(context.Background(), s, "etablissement", "x", 0); err == nil {
		t.Error("want unknown table error")
	}
	if es, _ := SearchNomenclature(context.Background(), s, "naf", "", 3); len(es) != 3 {
		t.Errorf("limit: %d", len(es))
	}
}

func TestSearchNomenclatureNoTruncation(t *testing.T) {
	s := testStore(t)
	for table, want := range map[string]int{"naf": 732, "categorie_juridique": 260, "tranche_effectifs": 16} {
		es, err := SearchNomenclature(context.Background(), s, table, "", 0)
		if err != nil || len(es) != want {
			t.Errorf("%s: empty text gave %d entries (%v), want the whole table (%d)", table, len(es), err, want)
		}
	}
	// "commerce" matches far more than the old silent cap of 50.
	es, err := SearchNomenclature(context.Background(), s, "naf", "commerce", 0)
	if err != nil || len(es) <= 50 {
		t.Errorf("commerce: %d entries (%v), want > 50", len(es), err)
	}
}

func TestSearchNomenclatureAliases(t *testing.T) {
	s := testStore(t)
	for _, tc := range []struct {
		table, text, code, label string
	}{
		{"naf", "kiné", "86.90E", "Activités des professionnels de la rééducation, de l'appareillage et des pédicures-podologues"}, //nolint:misspell // French
		{"naf", "KINESITHERAPIE", "86.90E", ""},
		{"naf", "kinésithérapeute", "86.90E", ""},
		{"naf", "ostéopathe", "86.90E", ""},
		{"naf", "plombier", "43.22A", "Travaux d'installation d'eau et de gaz en tous locaux"},
		{"naf", "expert comptable", "69.20Z", ""},
		{"naf", "médecin", "86.21Z", ""},
		{"naf2025", "kiné", "86.95Y", "Activités de physiothérapie"}, //nolint:misspell // French
	} {
		es, err := SearchNomenclature(context.Background(), s, tc.table, tc.text, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range es {
			if e.Code == tc.code {
				found = true
				if tc.label != "" && e.Label != tc.label {
					t.Errorf("%+v: label %q, want the official one", tc, e.Label)
				}
			}
		}
		if !found {
			t.Errorf("%+v: not found in %v", tc, es)
		}
	}
	// Aliases do not leak into unrelated searches.
	es, _ := SearchNomenclature(context.Background(), s, "naf", "kiné", 0)
	if len(es) != 1 {
		t.Errorf("kiné: %v, want only 86.90E", es)
	}
}

func TestGetInfoIgnoresUnfinishedSync(t *testing.T) {
	ul, etab := sirenetest.Default()
	dir := t.TempDir()
	ulPath, etabPath := sirenetest.Write(t, dir, ul, etab)
	out := filepath.Join(dir, "sirene.duckdb")
	if _, err := syncer.Build(context.Background(), syncer.BuildOptions{
		ULParquet: ulPath, EtabParquet: etabPath, OutPath: out,
		MemoryLimit: "1GB", TempDir: filepath.Join(dir, "tmp"),
	}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("duckdb", out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE sync_log SET finished_at = NULL"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := store.Open(out)
	defer s.Close()
	if _, err := GetInfo(context.Background(), s); err == nil || !strings.Contains(err.Error(), "no completed sync") {
		t.Fatalf("got %v", err)
	}
}

func TestGetInfo(t *testing.T) {
	i, err := GetInfo(context.Background(), testStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if i.ULRows != 5 || i.EtabRows != 6 || i.LastSync.IsZero() || !strings.HasPrefix(i.MaxDateDernierTraitement, "2024-05-02") {
		t.Fatalf("got %+v", i)
	}
}

var kinesGard = Filters{NAF: []string{"86.90E"}, Departements: []string{"30"}}

func TestExportCSV(t *testing.T) {
	s := testStore(t)
	dir := filepath.Join(t.TempDir(), "exports")
	r, err := Export(context.Background(), s, kinesGard, "csv", dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Rows != 2 || r.Format != "csv" || filepath.Dir(r.Path) != dir || !strings.HasPrefix(filepath.Base(r.Path), "sirene_") {
		t.Fatalf("got %+v", r)
	}
	f, err := os.Open(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "siret,siren,nom_affiche,") {
		t.Fatalf("lines = %q", lines)
	}
	// Never overwrite: a second export in the same second gets another name.
	r2, err := Export(context.Background(), s, kinesGard, "csv", dir)
	if err != nil || r2.Path == r.Path {
		t.Fatalf("second export %+v err=%v", r2, err)
	}
	if _, err := Export(context.Background(), s, kinesGard, "xlsx", dir); err == nil {
		t.Error("want format error")
	}
}

func TestExportJSON(t *testing.T) {
	r, err := Export(context.Background(), testStore(t), kinesGard, "json", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	if len(rows) != 2 || r.Rows != 2 || !strings.HasSuffix(r.Path, ".json") {
		t.Fatalf("rows = %v", rows)
	}
	if _, ok := rows[0]["diffusion_partielle"].(bool); !ok || rows[0]["siret"] == "" {
		t.Errorf("row = %v", rows[0])
	}
}

func TestRunSQLSelect(t *testing.T) {
	r, err := RunSQL(context.Background(), testStore(t), "SELECT count(*) AS n FROM unite_legale", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns) != 1 || r.Columns[0] != "n" || len(r.Rows) != 1 || r.Truncated {
		t.Fatalf("got %+v", r)
	}
	if n, ok := r.Rows[0][0].(int64); !ok || n != 5 {
		t.Fatalf("value %T %v", r.Rows[0][0], r.Rows[0][0])
	}
}

func TestRunSQLWithAndTypes(t *testing.T) {
	s := testStore(t)
	r, err := RunSQL(context.Background(), s, `-- comment
WITH x AS (SELECT siren, date_creation, date_dernier_traitement FROM unite_legale WHERE siren = '111111111')
SELECT * FROM x UNION ALL SELECT siren, date_creation, date_dernier_traitement FROM x`, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 2 {
		t.Fatalf("got %+v", r)
	}
	if v, ok := r.Rows[0][1].(string); !ok || !strings.HasPrefix(v, "2010-05-01") {
		t.Errorf("date %T %v", r.Rows[0][1], r.Rows[0][1])
	}
	if _, err := time.Parse(time.RFC3339, r.Rows[0][2].(string)); err != nil {
		t.Errorf("timestamp: %v", err)
	}
	if _, err := json.Marshal(r); err != nil {
		t.Errorf("marshal: %v", err)
	}
}

func TestRunSQLTruncates(t *testing.T) {
	r, err := RunSQL(context.Background(), testStore(t), "SELECT * FROM etablissement", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 2 || !r.Truncated {
		t.Fatalf("rows=%d truncated=%v", len(r.Rows), r.Truncated)
	}
}

func TestRunSQLTimeout(t *testing.T) {
	_, err := RunSQL(context.Background(), testStore(t),
		"SELECT count(*) FROM range(100000000000) a", 0, 50*time.Millisecond)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
}

func TestRunSQLRefuses(t *testing.T) {
	s := testStore(t)
	for _, q := range []string{
		"DELETE FROM naf",
		"SELECT 1; SELECT 2",
		"SELECT * FROM read_csv('/etc/hosts')",
		"COPY naf TO '/tmp/x.csv'",
		"ATTACH '/tmp/x.duckdb'",
		"INSTALL httpfs",
		"SET enable_external_access=true",
		"USE memory",
		"DETACH sirene",
		"PRAGMA version",
		"CALL pragma_version()",
		"SELECT * FROM read_text('/etc/hosts')",
		"EXPLAIN SELECT 1",
		"DESCRIBE naf",
		"FROM naf",
		"",
		"SELECT",
		"WITH a AS (DELETE FROM naf RETURNING *) SELECT * FROM a",
		"WITH a AS (UPDATE naf SET libelle='x' RETURNING *) SELECT * FROM a",
		"WITH a AS (INSERT INTO memory.main.t VALUES (1) RETURNING *) SELECT * FROM a",
		"SELECT 1 WHERE 1 IN (WITH b AS (DELETE FROM naf RETURNING 1) SELECT * FROM b)",
		"SELECT * FROM (WITH b AS (UPDATE naf SET libelle='x' RETURNING code) SELECT * FROM b) s",
	} {
		if _, err := RunSQL(context.Background(), s, q, 0, 0); err == nil {
			t.Errorf("%q: want error", q)
		}
	}
	// The pool still works afterwards, and nothing was deleted.
	r, err := RunSQL(context.Background(), s, "SELECT count(*) FROM naf WHERE libelle <> 'x'", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := r.Rows[0][0].(int64); n < 700 {
		t.Fatalf("naf rows = %d", n)
	}
}

// TestCheckSelect exercises the DuckDB parser gate alone, without the
// first-keyword pre-check.
func TestCheckSelect(t *testing.T) {
	s := testStore(t)
	err := s.UseLocked(context.Background(), func(db *sql.DB) error {
		for _, q := range []string{
			"DELETE FROM naf", "SELECT 1; SELECT 2", "SELECT 1; DROP TABLE naf", "COPY naf TO '/tmp/x.csv'",
			"ATTACH '/tmp/x.duckdb'", "INSTALL httpfs", "SET enable_external_access=true", "USE memory",
			"DETACH sirene", "PRAGMA version", "CALL pragma_version()", "EXPLAIN SELECT 1",
			"WITH a AS (SELECT 1) DELETE FROM naf",
			"WITH a AS (DELETE FROM naf RETURNING *) SELECT * FROM a",
			"WITH a AS (UPDATE naf SET libelle='x' RETURNING *) SELECT * FROM a",
			"WITH a AS (INSERT INTO memory.main.t VALUES (1) RETURNING *) SELECT * FROM a",
			"SELECT 1 WHERE 1 IN (WITH b AS (DELETE FROM naf RETURNING 1) SELECT * FROM b)",
			"SELECT * FROM (WITH b AS (UPDATE naf SET libelle='x' RETURNING code) SELECT * FROM b) s",
			"CREATE TABLE x AS SELECT 1", "garbage(",
		} {
			if err := checkSelect(context.Background(), db, q); err == nil {
				t.Errorf("%q: want error", q)
			}
		}
		for _, q := range []string{
			"SELECT 1", "WITH a AS (SELECT 1) SELECT * FROM a", "SELECT 1 UNION SELECT 2",
			"SELECT 1 EXCEPT SELECT 2 INTERSECT SELECT 3", "WITH a AS MATERIALIZED (SELECT 1) SELECT * FROM a",
			"WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n<3) SELECT * FROM r",
			"SELECT 'DELETE_QUERY_NODE' AS type, 'returning_list' AS returning_list",
		} {
			if err := checkSelect(context.Background(), db, q); err != nil {
				t.Errorf("%q: %v", q, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFirstKeyword(t *testing.T) {
	for q, want := range map[string]string{
		"select 1": "SELECT", "  -- c\n/* x */ (WITH a": "WITH", "-- only": "", "/* open": "", "": "",
	} {
		if got := firstKeyword(q); got != want {
			t.Errorf("%q = %q, want %q", q, got, want)
		}
	}
}
