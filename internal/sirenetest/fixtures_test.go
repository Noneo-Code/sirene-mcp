package sirenetest

import (
	"database/sql"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
)

func TestWriteFixturesReadable(t *testing.T) {
	ul, etab := Default()
	ulPath, etabPath := Write(t, t.TempDir(), ul, etab)

	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	count := func(path string) int {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM read_parquet(?)", path).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(ulPath); n != 5 {
		t.Errorf("UL rows = %d, want 5", n)
	}
	if n := count(etabPath); n != 6 {
		t.Errorf("etab rows = %d, want 6", n)
	}

	types := map[string]string{}
	rows, err := db.Query("DESCRIBE SELECT * FROM read_parquet(?)", etabPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, typ string
		var a, b, c, d sql.NullString
		if err := rows.Scan(&name, &typ, &a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		types[name] = typ
	}
	if types["activitePrincipaleNAF25Etablissement"] != "VARCHAR" || types["statutDiffusionEtablissement"] != "VARCHAR" ||
		types["etablissementSiege"] != "BOOLEAN" || types["dateCreationEtablissement"] != "DATE" {
		t.Errorf("unexpected types: %v", types)
	}
	if len(types) != 54 {
		t.Errorf("etab columns = %d, want 54", len(types))
	}

	var codeJur int
	if err := db.QueryRow("SELECT categorieJuridiqueUniteLegale FROM read_parquet(?) WHERE siren='111111111'", ulPath).Scan(&codeJur); err != nil || codeJur != 5499 {
		t.Errorf("categorieJuridique = %d, %v", codeJur, err)
	}
}
