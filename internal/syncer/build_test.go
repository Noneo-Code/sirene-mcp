package syncer

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Noneo-Code/sirene-mcp/internal/sirenetest"
)

func build(t *testing.T, ul []sirenetest.UL, etab []sirenetest.Etab, excludeClosed bool) (BuildStats, string) {
	t.Helper()
	dir := t.TempDir()
	ulPath, etabPath := sirenetest.Write(t, dir, ul, etab)
	out := filepath.Join(dir, "sirene.duckdb")
	stats, err := Build(context.Background(), BuildOptions{
		ULParquet: ulPath, EtabParquet: etabPath, OutPath: out,
		ExcludeClosed: excludeClosed, MemoryLimit: "1GB", TempDir: filepath.Join(dir, "tmp"),
		Sources: SourceInfo{ULURL: "https://example/ul", EtabURL: "https://example/etab",
			ULLastModified: "Mon, 03 Nov 2026 00:00:00 GMT", EtabLastModified: "Mon, 03 Nov 2026 00:00:00 GMT"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return stats, out
}

func buildDefault(t *testing.T, excludeClosed bool) (BuildStats, string) {
	t.Helper()
	ul, etab := sirenetest.Default()
	return build(t, ul, etab, excludeClosed)
}

func openRO(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("duckdb", path+"?access_mode=read_only")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func queryRow(t *testing.T, db *sql.DB, q string, args []any, dest ...any) {
	t.Helper()
	if err := db.QueryRow(q, args...).Scan(dest...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func TestBuildCounts(t *testing.T) {
	stats, out := buildDefault(t, false)
	if stats.ULRows != 5 || stats.EtabRows != 6 {
		t.Fatalf("stats = %+v, want 5/6", stats)
	}
	db := openRO(t, out)
	var ul, etab, logs int64
	queryRow(t, db, "SELECT count(*) FROM unite_legale", nil, &ul)
	queryRow(t, db, "SELECT count(*) FROM etablissement", nil, &etab)
	if ul != 5 || etab != 6 {
		t.Fatalf("tables = %d/%d, want 5/6", ul, etab)
	}
	var excl bool
	var ulRows, etabRows int64
	var ulSrc, lm string
	queryRow(t, db, "SELECT count(*) FROM sync_log", nil, &logs)
	if logs != 1 {
		t.Fatalf("sync_log rows = %d, want 1", logs)
	}
	queryRow(t, db, `SELECT exclude_closed, ul_rows, etab_rows, ul_source, ul_last_modified FROM sync_log
		WHERE started_at <= finished_at`, nil, &excl, &ulRows, &etabRows, &ulSrc, &lm)
	if excl || ulRows != 5 || etabRows != 6 || ulSrc != "https://example/ul" || lm == "" {
		t.Fatalf("sync_log = %v %d %d %q %q", excl, ulRows, etabRows, ulSrc, lm)
	}
}

func TestBuildDerivedColumns(t *testing.T) {
	_, out := buildDefault(t, false)
	db := openRO(t, out)
	var dep, nomAffiche, nomRecherche, catJur, adresse string
	var siege bool
	queryRow(t, db, `SELECT departement, nom_affiche, nom_recherche, categorie_juridique, est_siege, adresse_complete
		FROM etablissement WHERE siret = ?`, []any{"11111111100011"},
		&dep, &nomAffiche, &nomRecherche, &catJur, &siege, &adresse)
	if dep != "30" {
		t.Errorf("departement = %q", dep)
	}
	// The fixture's siège carries enseigne1Etablissement = "CABINET KINE SUD", which
	// comes first in the nom_affiche rule (spec §6: enseigne, then dénomination
	// usuelle, then UL dénomination, then prénom + nom).
	if nomAffiche != "CABINET KINE SUD" {
		t.Errorf("nom_affiche = %q", nomAffiche)
	}
	if !strings.Contains(nomRecherche, "kine sud") {
		t.Errorf("nom_recherche = %q", nomRecherche)
	}
	if catJur != "5499" {
		t.Errorf("categorie_juridique = %q", catJur)
	}
	if !siege {
		t.Error("est_siege = false")
	}
	if !strings.Contains(adresse, "30000") || strings.Contains(adresse, "  ") {
		t.Errorf("adresse_complete = %q", adresse)
	}
	if adresse != "12 RUE DE LA MAISON CARREE 30000 NIMES" {
		t.Errorf("adresse_complete = %q", adresse)
	}

	// UL-name fallback (no enseigne) and the unite_legale side.
	queryRow(t, db, `SELECT nom_affiche FROM etablissement WHERE siret = ?`, []any{"55555555500015"}, &nomAffiche)
	if nomAffiche != "Soins Réunion" {
		t.Errorf("nom_affiche fallback = %q", nomAffiche)
	}
	var ulCat string
	queryRow(t, db, `SELECT categorie_juridique FROM unite_legale WHERE siren = ?`, []any{"222222222"}, &ulCat)
	if ulCat != "1000" {
		t.Errorf("unite_legale.categorie_juridique = %q", ulCat)
	}
}

func TestBuildDepartementEdgeCases(t *testing.T) {
	ul, etab := sirenetest.Default()
	etab = append(etab, sirenetest.Etab{
		"siren": "111111111", "nic": "00037", "siret": "11111111100037", "statutDiffusionEtablissement": "O",
		"etablissementSiege": "false", "numeroVoieEtablissement": "1", "libelleVoieEtablissement": "RUE ROYALE",
		"libelleCommuneEtrangerEtablissement": "BRUXELLES", "codePaysEtrangerEtablissement": "99131",
		"libellePaysEtrangerEtablissement": "BELGIQUE", "codeCommuneEtablissement": "",
		"etatAdministratifEtablissement": "A", "activitePrincipaleEtablissement": "86.90E",
	})
	_, out := build(t, ul, etab, false)
	db := openRO(t, out)
	for siret, want := range map[string]string{"44444444400027": "2A", "55555555500015": "974"} {
		var dep string
		queryRow(t, db, "SELECT departement FROM etablissement WHERE siret = ?", []any{siret}, &dep)
		if dep != want {
			t.Errorf("%s departement = %q, want %q", siret, dep, want)
		}
	}
	var dep sql.NullString
	var adresse string
	queryRow(t, db, "SELECT departement, adresse_complete FROM etablissement WHERE siret = ?",
		[]any{"11111111100037"}, &dep, &adresse)
	if dep.Valid {
		t.Errorf("foreign departement = %q, want NULL", dep.String)
	}
	if !strings.Contains(adresse, "BELGIQUE") || strings.Contains(adresse, "  ") {
		t.Errorf("foreign adresse_complete = %q", adresse)
	}
}

func TestBuildKeepsPartialDiffusion(t *testing.T) {
	_, out := buildDefault(t, false)
	db := openRO(t, out)
	var statut, voie string
	queryRow(t, db, "SELECT statut_diffusion, libelle_voie FROM etablissement WHERE siret = ?",
		[]any{"22222222200013"}, &statut, &voie)
	if statut != "P" || voie != "[ND]" {
		t.Fatalf("statut_diffusion = %q, libelle_voie = %q", statut, voie)
	}
}

func TestBuildExcludeClosed(t *testing.T) {
	stats, out := buildDefault(t, true)
	if stats.ULRows != 4 || stats.EtabRows != 4 {
		t.Fatalf("stats = %+v, want 4/4", stats)
	}
	db := openRO(t, out)
	var n int64
	queryRow(t, db, "SELECT count(*) FROM etablissement WHERE siret IN ('11111111100029', '44444444400027')", nil, &n)
	if n != 0 {
		t.Errorf("closed establishments kept: %d", n)
	}
	var excl bool
	var etabRows int64
	queryRow(t, db, "SELECT exclude_closed, etab_rows FROM sync_log", nil, &excl, &etabRows)
	if !excl || etabRows != 4 {
		t.Errorf("sync_log exclude_closed = %v, etab_rows = %d", excl, etabRows)
	}
}

func TestBuildNomenclatures(t *testing.T) {
	_, out := buildDefault(t, false)
	db := openRO(t, out)
	var naf, tranches, naf25, cj int64
	queryRow(t, db, "SELECT count(*) FROM naf", nil, &naf)
	queryRow(t, db, "SELECT count(*) FROM tranche_effectifs", nil, &tranches)
	queryRow(t, db, "SELECT count(*) FROM naf2025", nil, &naf25)
	queryRow(t, db, "SELECT count(*) FROM categorie_juridique", nil, &cj)
	if naf != 732 || tranches != 16 || naf25 == 0 || cj == 0 {
		t.Fatalf("naf = %d, tranche_effectifs = %d, naf2025 = %d, categorie_juridique = %d", naf, tranches, naf25, cj)
	}
	var label string
	queryRow(t, db, "SELECT libelle FROM categorie_juridique WHERE code = '5499'", nil, &label)
	if label == "" {
		t.Error("categorie_juridique 5499 has no label")
	}
}

func TestBuildRefusesExistingOut(t *testing.T) {
	dir := t.TempDir()
	ul, etab := sirenetest.Default()
	ulPath, etabPath := sirenetest.Write(t, dir, ul, etab)
	out := filepath.Join(dir, "sirene.duckdb")
	if err := os.WriteFile(out, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), BuildOptions{ULParquet: ulPath, EtabParquet: etabPath, OutPath: out}); err == nil {
		t.Fatal("Build succeeded over an existing file")
	}
	b, err := os.ReadFile(out)
	if err != nil || string(b) != "keep me" {
		t.Fatalf("existing file changed: %q, %v", b, err)
	}
}

func TestBuildRemovesOutOnError(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "sirene.duckdb")
	_, err := Build(context.Background(), BuildOptions{
		ULParquet: filepath.Join(dir, "missing-ul.parquet"), EtabParquet: filepath.Join(dir, "missing-etab.parquet"), OutPath: out,
	})
	if err == nil {
		t.Fatal("Build succeeded with missing inputs")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("output left behind: %v", statErr)
	}
}
