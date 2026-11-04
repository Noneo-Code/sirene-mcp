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
	} {
		if _, err := RunSQL(context.Background(), s, q, 0, 0); err == nil {
			t.Errorf("%q: want error", q)
		}
	}
	// The pool still works afterwards.
	if _, err := RunSQL(context.Background(), s, "SELECT 1", 0, 0); err != nil {
		t.Fatal(err)
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
			"WITH a AS (SELECT 1) DELETE FROM naf", "CREATE TABLE x AS SELECT 1", "garbage(",
		} {
			if err := checkSelect(context.Background(), db, q); err == nil {
				t.Errorf("%q: want error", q)
			}
		}
		for _, q := range []string{"SELECT 1", "WITH a AS (SELECT 1) SELECT * FROM a", "SELECT 1 UNION SELECT 2"} {
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
