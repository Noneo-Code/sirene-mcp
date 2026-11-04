// Package syncer downloads the Sirene stocks and builds the DuckDB database.
package syncer

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/duckdb/duckdb-go/v2"

	"github.com/Noneo-Code/sirene-mcp/internal/nomenclature"
)

// BuildOptions configures Build.
type BuildOptions struct {
	ULParquet, EtabParquet string
	OutPath                string // created from scratch; must not exist
	ExcludeClosed          bool
	MemoryLimit            string // DuckDB syntax, e.g. "4GB"; "" = DuckDB default
	TempDir                string
	Sources                SourceInfo // recorded in sync_log
}

// SourceInfo describes where the stocks came from; it is recorded in sync_log.
type SourceInfo struct{ ULURL, EtabURL, ULLastModified, EtabLastModified string }

// BuildStats are the row counts actually loaded (after the exclude-closed filter).
type BuildStats struct{ ULRows, EtabRows int64 }

// Build creates a new DuckDB database at o.OutPath from the two Sirene Parquet
// stocks. OutPath must not exist; on any error, the partial output is removed.
func Build(ctx context.Context, o BuildOptions) (stats BuildStats, err error) {
	if _, statErr := os.Lstat(o.OutPath); statErr == nil {
		return stats, fmt.Errorf("build: %s already exists", o.OutPath)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return stats, fmt.Errorf("build: %w", statErr)
	}
	started := time.Now().UTC()

	db, err := sql.Open("duckdb", o.OutPath)
	if err != nil {
		return stats, fmt.Errorf("build: open %s: %w", o.OutPath, err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("build: close: %w", cerr)
		}
		if err != nil {
			_ = os.Remove(o.OutPath)
			_ = os.Remove(o.OutPath + ".wal")
		}
	}()

	conn, err := db.Conn(ctx)
	if err != nil {
		return stats, fmt.Errorf("build: %w", err)
	}
	defer conn.Close()

	exec := func(step, q string, args ...any) error {
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("build: %s: %w", step, err)
		}
		return nil
	}

	if o.MemoryLimit != "" {
		if err := exec("memory_limit", "SET memory_limit = "+quote(o.MemoryLimit)); err != nil {
			return stats, err
		}
	}
	if o.TempDir != "" {
		if err := exec("temp_directory", "SET temp_directory = "+quote(o.TempDir)); err != nil {
			return stats, err
		}
	}
	if err := exec("preserve_insertion_order", "SET preserve_insertion_order = false"); err != nil {
		return stats, err
	}

	if err := exec("unite_legale", uniteLegaleSQL(o.ULParquet, o.ExcludeClosed)); err != nil {
		return stats, err
	}
	if err := exec("etablissement", etablissementSQL(o.EtabParquet, o.ExcludeClosed)); err != nil {
		return stats, err
	}
	if err := loadNomenclatures(ctx, conn); err != nil {
		return stats, err
	}
	for _, q := range []string{
		"CREATE INDEX etablissement_siret ON etablissement (siret)",
		"CREATE INDEX etablissement_siren ON etablissement (siren)",
		"CREATE INDEX unite_legale_siren ON unite_legale (siren)",
	} {
		if err := exec("index", q); err != nil {
			return stats, err
		}
	}

	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM unite_legale").Scan(&stats.ULRows); err != nil {
		return stats, fmt.Errorf("build: count unite_legale: %w", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM etablissement").Scan(&stats.EtabRows); err != nil {
		return stats, fmt.Errorf("build: count etablissement: %w", err)
	}

	if err := exec("sync_log", `CREATE TABLE sync_log (
		started_at TIMESTAMP, finished_at TIMESTAMP, ul_rows BIGINT, etab_rows BIGINT,
		exclude_closed BOOLEAN, ul_source VARCHAR, etab_source VARCHAR,
		ul_last_modified VARCHAR, etab_last_modified VARCHAR)`); err != nil {
		return stats, err
	}
	s := o.Sources
	if err := exec("sync_log", "INSERT INTO sync_log VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		started, time.Now().UTC(), stats.ULRows, stats.EtabRows, o.ExcludeClosed,
		s.ULURL, s.EtabURL, s.ULLastModified, s.EtabLastModified); err != nil {
		return stats, err
	}
	if err := exec("checkpoint", "CHECKPOINT"); err != nil {
		return stats, err
	}
	return stats, nil
}

// quote returns s as a SQL string literal.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// txt cleans a Sirene text column: trimmed, empty → NULL.
func txt(col string) string { return fmt.Sprintf("nullif(trim(%q), '')", col) }

func uniteLegaleSQL(path string, excludeClosed bool) string {
	where := ""
	if excludeClosed {
		where = `WHERE "etatAdministratifUniteLegale" IS DISTINCT FROM 'C'`
	}
	return fmt.Sprintf(`CREATE TABLE unite_legale AS
SELECT
	%s AS siren,
	%s AS statut_diffusion,
	%s AS denomination,
	%s AS sigle,
	%s AS nom,
	%s AS nom_usage,
	coalesce(%s, %s) AS prenom,
	lpad(CAST("categorieJuridiqueUniteLegale" AS VARCHAR), 4, '0') AS categorie_juridique,
	%s AS activite_principale,
	%s AS activite_principale_naf25,
	%s AS etat_administratif,
	%s AS categorie_entreprise,
	%s AS tranche_effectifs,
	%s AS economie_sociale_solidaire,
	%s AS nic_siege,
	TRY_CAST("dateCreationUniteLegale" AS DATE) AS date_creation,
	TRY_CAST("dateDernierTraitementUniteLegale" AS TIMESTAMP) AS date_dernier_traitement
FROM read_parquet(%s)
%s`,
		txt("siren"), txt("statutDiffusionUniteLegale"), txt("denominationUniteLegale"),
		txt("sigleUniteLegale"), txt("nomUniteLegale"), txt("nomUsageUniteLegale"),
		txt("prenomUsuelUniteLegale"), txt("prenom1UniteLegale"),
		txt("activitePrincipaleUniteLegale"), txt("activitePrincipaleNAF25UniteLegale"),
		txt("etatAdministratifUniteLegale"), txt("categorieEntreprise"),
		txt("trancheEffectifsUniteLegale"), txt("economieSocialeSolidaireUniteLegale"),
		txt("nicSiegeUniteLegale"), quote(path), where)
}

func etablissementSQL(path string, excludeClosed bool) string {
	where := ""
	if excludeClosed {
		where = `WHERE "etatAdministratifEtablissement" IS DISTINCT FROM 'F'`
	}
	return fmt.Sprintf(`CREATE TABLE etablissement AS
WITH e AS (
	SELECT
		%s AS siret,
		%s AS siren,
		%s AS nic,
		"etablissementSiege" AS est_siege,
		%s AS etat_administratif,
		%s AS statut_diffusion,
		%s AS enseigne,
		%s AS denomination_usuelle,
		%s AS activite_principale,
		%s AS nomenclature_activite,
		%s AS activite_principale_naf25,
		%s AS numero_voie,
		%s AS indice_repetition,
		%s AS type_voie,
		%s AS libelle_voie,
		%s AS complement_adresse,
		%s AS code_postal,
		%s AS code_commune,
		%s AS libelle_commune,
		%s AS libelle_commune_etranger,
		%s AS libelle_pays_etranger,
		%s AS tranche_effectifs,
		%s AS caractere_employeur,
		TRY_CAST("dateCreationEtablissement" AS DATE) AS date_creation,
		TRY_CAST("dateDernierTraitementEtablissement" AS TIMESTAMP) AS date_dernier_traitement
	FROM read_parquet(%s)
	%s
), n AS (
	SELECT
		e.*,
		u.categorie_juridique,
		u.categorie_entreprise,
		u.sigle AS ul_sigle,
		u.denomination AS ul_denomination,
		coalesce(
			e.enseigne,
			e.denomination_usuelle,
			u.denomination,
			nullif(trim(concat_ws(' ', u.prenom, coalesce(u.nom_usage, u.nom))), '')
		) AS nom_affiche
	FROM e LEFT JOIN unite_legale u USING (siren)
)
SELECT
	siret, siren, nic, est_siege, etat_administratif, statut_diffusion, enseigne,
	denomination_usuelle, activite_principale, nomenclature_activite, activite_principale_naf25,
	numero_voie, indice_repetition, type_voie, libelle_voie, complement_adresse, code_postal,
	code_commune, libelle_commune, libelle_pays_etranger, tranche_effectifs, caractere_employeur,
	date_creation, date_dernier_traitement,
	CASE
		WHEN code_commune IS NULL THEN NULL
		WHEN code_commune LIKE '97%%' THEN upper(left(code_commune, 3))
		ELSE upper(left(code_commune, 2))
	END AS departement,
	nullif(concat_ws(' ', numero_voie, indice_repetition, type_voie, libelle_voie, complement_adresse,
		code_postal, coalesce(libelle_commune, libelle_commune_etranger), libelle_pays_etranger), '') AS adresse_complete,
	nom_affiche,
	strip_accents(lower(concat_ws(' ', nom_affiche, ul_sigle, ul_denomination))) AS nom_recherche,
	categorie_juridique,
	categorie_entreprise
FROM n
ORDER BY departement, activite_principale`,
		txt("siret"), txt("siren"), txt("nic"),
		txt("etatAdministratifEtablissement"), txt("statutDiffusionEtablissement"),
		txt("enseigne1Etablissement"), txt("denominationUsuelleEtablissement"),
		txt("activitePrincipaleEtablissement"), txt("nomenclatureActivitePrincipaleEtablissement"),
		txt("activitePrincipaleNAF25Etablissement"),
		txt("numeroVoieEtablissement"), txt("indiceRepetitionEtablissement"),
		txt("typeVoieEtablissement"), txt("libelleVoieEtablissement"),
		txt("complementAdresseEtablissement"), txt("codePostalEtablissement"),
		txt("codeCommuneEtablissement"), txt("libelleCommuneEtablissement"),
		txt("libelleCommuneEtrangerEtablissement"), txt("libellePaysEtrangerEtablissement"),
		txt("trancheEffectifsEtablissement"), txt("caractereEmployeurEtablissement"),
		quote(path), where)
}

func loadNomenclatures(ctx context.Context, conn *sql.Conn) error {
	tables := nomenclature.Tables()
	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s (code VARCHAR, libelle VARCHAR)", name)); err != nil {
			return fmt.Errorf("build: nomenclature %s: %w", name, err)
		}
		err := conn.Raw(func(dc any) error {
			app, err := duckdb.NewAppenderFromConn(dc.(driver.Conn), "", name)
			if err != nil {
				return err
			}
			for _, e := range tables[name] {
				if err := app.AppendRow(e.Code, e.Label); err != nil {
					_ = app.Close()
					return err
				}
			}
			return app.Close()
		})
		if err != nil {
			return fmt.Errorf("build: nomenclature %s: %w", name, err)
		}
	}
	return nil
}
