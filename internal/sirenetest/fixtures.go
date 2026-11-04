// Package sirenetest writes small Sirene Parquet fixtures for tests.
//
// Column types observed in the real stock files (data.gouv.fr, dessins de
// fichier v3.11, read with DuckDB on 2026-11-04):
//
//	UL (35 columns): every column is VARCHAR except
//	  unitePurgeeUniteLegale BOOLEAN, dateCreationUniteLegale DATE, dateDebut DATE,
//	  anneeEffectifsUniteLegale BIGINT, nombrePeriodesUniteLegale BIGINT,
//	  anneeCategorieEntreprise BIGINT, categorieJuridiqueUniteLegale BIGINT,
//	  dateDernierTraitementUniteLegale TIMESTAMP.
//	Etablissement (54 columns): every column is VARCHAR except
//	  dateCreationEtablissement DATE, dateDebut DATE, etablissementSiege BOOLEAN,
//	  anneeEffectifsEtablissement BIGINT, nombrePeriodesEtablissement BIGINT,
//	  dateDernierTraitementEtablissement TIMESTAMP.
//
// Note that categorieJuridiqueUniteLegale is a number in the stock (no leading
// zeros are lost: codes are 4 digits starting with 1-9).
package sirenetest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2" // DuckDB driver
)

// UL is one unité légale row, keyed by Sirene column name. Missing keys are NULL.
type UL map[string]string

// Etab is one établissement row, keyed by Sirene column name. Missing keys are NULL.
type Etab map[string]string

type column struct{ name, typ string }

func cols(typed map[string]string, names string) []column {
	out := []column{}
	for _, n := range strings.Fields(names) {
		t := typed[n]
		if t == "" {
			t = "VARCHAR"
		}
		out = append(out, column{n, t})
	}
	return out
}

var ulColumns = cols(map[string]string{
	"unitePurgeeUniteLegale":           "BOOLEAN",
	"dateCreationUniteLegale":          "DATE",
	"dateDebut":                        "DATE",
	"anneeEffectifsUniteLegale":        "BIGINT",
	"nombrePeriodesUniteLegale":        "BIGINT",
	"anneeCategorieEntreprise":         "BIGINT",
	"categorieJuridiqueUniteLegale":    "BIGINT",
	"dateDernierTraitementUniteLegale": "TIMESTAMP",
}, `siren statutDiffusionUniteLegale unitePurgeeUniteLegale dateCreationUniteLegale sigleUniteLegale
sexeUniteLegale prenom1UniteLegale prenom2UniteLegale prenom3UniteLegale prenom4UniteLegale
prenomUsuelUniteLegale pseudonymeUniteLegale identifiantAssociationUniteLegale trancheEffectifsUniteLegale
anneeEffectifsUniteLegale dateDernierTraitementUniteLegale nombrePeriodesUniteLegale categorieEntreprise
anneeCategorieEntreprise dateDebut etatAdministratifUniteLegale nomUniteLegale nomUsageUniteLegale
denominationUniteLegale denominationUsuelle1UniteLegale denominationUsuelle2UniteLegale
denominationUsuelle3UniteLegale categorieJuridiqueUniteLegale activitePrincipaleUniteLegale
nomenclatureActivitePrincipaleUniteLegale nicSiegeUniteLegale economieSocialeSolidaireUniteLegale
societeMissionUniteLegale caractereEmployeurUniteLegale activitePrincipaleNAF25UniteLegale`)

var etabColumns = cols(map[string]string{
	"dateCreationEtablissement":          "DATE",
	"dateDebut":                          "DATE",
	"etablissementSiege":                 "BOOLEAN",
	"anneeEffectifsEtablissement":        "BIGINT",
	"nombrePeriodesEtablissement":        "BIGINT",
	"dateDernierTraitementEtablissement": "TIMESTAMP",
}, `siren nic siret statutDiffusionEtablissement dateCreationEtablissement trancheEffectifsEtablissement
anneeEffectifsEtablissement activitePrincipaleRegistreMetiersEtablissement dateDernierTraitementEtablissement
etablissementSiege nombrePeriodesEtablissement complementAdresseEtablissement numeroVoieEtablissement
indiceRepetitionEtablissement dernierNumeroVoieEtablissement indiceRepetitionDernierNumeroVoieEtablissement
typeVoieEtablissement libelleVoieEtablissement codePostalEtablissement libelleCommuneEtablissement
libelleCommuneEtrangerEtablissement distributionSpecialeEtablissement codeCommuneEtablissement
codeCedexEtablissement libelleCedexEtablissement codePaysEtrangerEtablissement libellePaysEtrangerEtablissement
identifiantAdresseEtablissement coordonneeLambertAbscisseEtablissement coordonneeLambertOrdonneeEtablissement
complementAdresse2Etablissement numeroVoie2Etablissement indiceRepetition2Etablissement typeVoie2Etablissement
libelleVoie2Etablissement codePostal2Etablissement libelleCommune2Etablissement
libelleCommuneEtranger2Etablissement distributionSpeciale2Etablissement codeCommune2Etablissement
codeCedex2Etablissement libelleCedex2Etablissement codePaysEtranger2Etablissement
libellePaysEtranger2Etablissement dateDebut etatAdministratifEtablissement enseigne1Etablissement
enseigne2Etablissement enseigne3Etablissement denominationUsuelleEtablissement activitePrincipaleEtablissement
nomenclatureActivitePrincipaleEtablissement caractereEmployeurEtablissement activitePrincipaleNAF25Etablissement`)

// Write writes unite_legale.parquet and etablissement.parquet into dir with every
// column of the v3.11 dessins, and returns their paths.
func Write(t testing.TB, dir string, ul []UL, etab []Etab) (ulPath, etabPath string) {
	t.Helper()
	ulRows := make([]map[string]string, len(ul))
	for i, r := range ul {
		ulRows[i] = r
	}
	etabRows := make([]map[string]string, len(etab))
	for i, r := range etab {
		etabRows[i] = r
	}
	ulPath = filepath.Join(dir, "unite_legale.parquet")
	etabPath = filepath.Join(dir, "etablissement.parquet")
	writeParquet(t, ulPath, "unite_legale", ulColumns, ulRows)
	writeParquet(t, etabPath, "etablissement", etabColumns, etabRows)
	return ulPath, etabPath
}

func writeParquet(t testing.TB, path, table string, columns []column, rows []map[string]string) {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // one connection so the in-memory database is shared

	defs := make([]string, len(columns))
	marks := make([]string, len(columns))
	for i, c := range columns {
		defs[i] = fmt.Sprintf("%q %s", c.name, c.typ)
		marks[i] = fmt.Sprintf("CAST(? AS %s)", c.typ)
	}
	if _, err := db.Exec(fmt.Sprintf("CREATE TABLE %s (%s)", table, strings.Join(defs, ", "))); err != nil {
		t.Fatal(err)
	}
	insert := fmt.Sprintf("INSERT INTO %s VALUES (%s)", table, strings.Join(marks, ", ")) //nolint:gosec // G201: test fixture, table name is a constant
	for _, r := range rows {
		args := make([]any, len(columns))
		for i, c := range columns {
			if v, ok := r[c.name]; ok {
				args[i] = v
			}
		}
		for k := range r {
			if !hasColumn(columns, k) {
				t.Fatalf("sirenetest: unknown %s column %q", table, k)
			}
		}
		if _, err := db.Exec(insert, args...); err != nil {
			t.Fatalf("insert into %s: %v", table, err)
		}
	}
	q := strings.ReplaceAll(path, "'", "''")
	if _, err := db.Exec(fmt.Sprintf("COPY %s TO '%s' (FORMAT parquet)", table, q)); err != nil {
		t.Fatal(err)
	}
}

func hasColumn(columns []column, name string) bool {
	for _, c := range columns {
		if c.name == name {
			return true
		}
	}
	return false
}

// Default returns the reference dataset shared by every test: 5 legal units and
// 6 establishments.
func Default() ([]UL, []Etab) {
	ul := []UL{
		{
			"siren": "111111111", "statutDiffusionUniteLegale": "O", "unitePurgeeUniteLegale": "false",
			"dateCreationUniteLegale": "2010-05-01", "trancheEffectifsUniteLegale": "03", "anneeEffectifsUniteLegale": "2023",
			"dateDernierTraitementUniteLegale": "2024-03-15 10:00:00", "nombrePeriodesUniteLegale": "1",
			"categorieEntreprise": "PME", "anneeCategorieEntreprise": "2023", "dateDebut": "2010-05-01",
			"etatAdministratifUniteLegale": "A", "denominationUniteLegale": "Kiné Sud", "sigleUniteLegale": "KS",
			"categorieJuridiqueUniteLegale": "5499", "activitePrincipaleUniteLegale": "86.90E",
			"nomenclatureActivitePrincipaleUniteLegale": "NAFRev2", "nicSiegeUniteLegale": "00011",
			"economieSocialeSolidaireUniteLegale": "N", "societeMissionUniteLegale": "N",
			"caractereEmployeurUniteLegale": "O", "activitePrincipaleNAF25UniteLegale": "86.95Z",
		},
		{
			"siren": "222222222", "statutDiffusionUniteLegale": "P", "unitePurgeeUniteLegale": "false",
			"dateCreationUniteLegale": "2015-09-14", "sexeUniteLegale": "F", "prenom1UniteLegale": "[ND]",
			"prenomUsuelUniteLegale": "[ND]", "dateDernierTraitementUniteLegale": "2024-01-10 08:30:00",
			"nombrePeriodesUniteLegale": "1", "dateDebut": "2015-09-14", "etatAdministratifUniteLegale": "A",
			"nomUniteLegale": "[ND]", "nomUsageUniteLegale": "[ND]", "categorieJuridiqueUniteLegale": "1000",
			"activitePrincipaleUniteLegale": "86.90E", "nomenclatureActivitePrincipaleUniteLegale": "NAFRev2",
			"nicSiegeUniteLegale": "00013", "economieSocialeSolidaireUniteLegale": "N",
			"caractereEmployeurUniteLegale": "N", "activitePrincipaleNAF25UniteLegale": "86.95Z",
		},
		{
			"siren": "333333333", "statutDiffusionUniteLegale": "O", "unitePurgeeUniteLegale": "false",
			"dateCreationUniteLegale": "2018-02-20", "trancheEffectifsUniteLegale": "11", "anneeEffectifsUniteLegale": "2023",
			"dateDernierTraitementUniteLegale": "2024-05-02 12:00:00", "nombrePeriodesUniteLegale": "2",
			"categorieEntreprise": "PME", "anneeCategorieEntreprise": "2023", "dateDebut": "2018-02-20",
			"etatAdministratifUniteLegale": "A", "denominationUniteLegale": "Boulangerie de l'Étang",
			"categorieJuridiqueUniteLegale": "5710", "activitePrincipaleUniteLegale": "10.71C",
			"nomenclatureActivitePrincipaleUniteLegale": "NAFRev2", "nicSiegeUniteLegale": "00019",
			"economieSocialeSolidaireUniteLegale": "N", "societeMissionUniteLegale": "N",
			"caractereEmployeurUniteLegale": "O", "activitePrincipaleNAF25UniteLegale": "10.71C",
		},
		{
			"siren": "444444444", "statutDiffusionUniteLegale": "O", "unitePurgeeUniteLegale": "false",
			"dateCreationUniteLegale": "2005-11-30", "dateDernierTraitementUniteLegale": "2022-06-01 09:00:00",
			"nombrePeriodesUniteLegale": "3", "dateDebut": "2020-12-31", "etatAdministratifUniteLegale": "C",
			"denominationUniteLegale": "Cabinet Corse Santé", "categorieJuridiqueUniteLegale": "5499",
			"activitePrincipaleUniteLegale": "86.90E", "nomenclatureActivitePrincipaleUniteLegale": "NAFRev2",
			"nicSiegeUniteLegale": "00027", "economieSocialeSolidaireUniteLegale": "N",
			"caractereEmployeurUniteLegale": "N", "activitePrincipaleNAF25UniteLegale": "86.95Z",
		},
		{
			"siren": "555555555", "statutDiffusionUniteLegale": "O", "unitePurgeeUniteLegale": "false",
			"dateCreationUniteLegale": "2021-03-08", "trancheEffectifsUniteLegale": "01", "anneeEffectifsUniteLegale": "2023",
			"dateDernierTraitementUniteLegale": "2024-02-20 14:00:00", "nombrePeriodesUniteLegale": "1",
			"categorieEntreprise": "PME", "anneeCategorieEntreprise": "2023", "dateDebut": "2021-03-08",
			"etatAdministratifUniteLegale": "A", "denominationUniteLegale": "Soins Réunion",
			"categorieJuridiqueUniteLegale": "5599", "activitePrincipaleUniteLegale": "86.90E",
			"nomenclatureActivitePrincipaleUniteLegale": "NAFRev2", "nicSiegeUniteLegale": "00015",
			"economieSocialeSolidaireUniteLegale": "N", "societeMissionUniteLegale": "N",
			"caractereEmployeurUniteLegale": "O", "activitePrincipaleNAF25UniteLegale": "86.95Z",
		},
	}
	etab := []Etab{
		{
			"siren": "111111111", "nic": "00011", "siret": "11111111100011", "statutDiffusionEtablissement": "O",
			"dateCreationEtablissement": "2010-05-01", "trancheEffectifsEtablissement": "03", "anneeEffectifsEtablissement": "2023",
			"dateDernierTraitementEtablissement": "2024-03-15 10:00:00", "etablissementSiege": "true",
			"nombrePeriodesEtablissement": "1", "numeroVoieEtablissement": "12", "typeVoieEtablissement": "RUE",
			"libelleVoieEtablissement": "DE LA MAISON CARREE", "codePostalEtablissement": "30000",
			"libelleCommuneEtablissement": "NIMES", "codeCommuneEtablissement": "30189", "dateDebut": "2010-05-01",
			"etatAdministratifEtablissement": "A", "enseigne1Etablissement": "CABINET KINE SUD",
			"activitePrincipaleEtablissement": "86.90E", "nomenclatureActivitePrincipaleEtablissement": "NAFRev2",
			"caractereEmployeurEtablissement": "O", "activitePrincipaleNAF25Etablissement": "86.95Z",
		},
		{
			"siren": "111111111", "nic": "00029", "siret": "11111111100029", "statutDiffusionEtablissement": "O",
			"dateCreationEtablissement": "2012-01-15", "dateDernierTraitementEtablissement": "2023-09-01 11:00:00",
			"etablissementSiege": "false", "nombrePeriodesEtablissement": "2", "numeroVoieEtablissement": "3",
			"indiceRepetitionEtablissement": "B", "typeVoieEtablissement": "AV", "libelleVoieEtablissement": "JEAN JAURES",
			"codePostalEtablissement": "30100", "libelleCommuneEtablissement": "ALES", "codeCommuneEtablissement": "30007",
			"dateDebut": "2019-06-30", "etatAdministratifEtablissement": "F",
			"activitePrincipaleEtablissement": "86.90E", "nomenclatureActivitePrincipaleEtablissement": "NAFRev2",
			"caractereEmployeurEtablissement": "N", "activitePrincipaleNAF25Etablissement": "86.95Z",
		},
		{
			"siren": "222222222", "nic": "00013", "siret": "22222222200013", "statutDiffusionEtablissement": "P",
			"dateCreationEtablissement": "2015-09-14", "dateDernierTraitementEtablissement": "2024-01-10 08:30:00",
			"etablissementSiege": "true", "nombrePeriodesEtablissement": "1", "numeroVoieEtablissement": "[ND]",
			"libelleVoieEtablissement": "[ND]", "codePostalEtablissement": "30700",
			"libelleCommuneEtablissement": "UZES", "codeCommuneEtablissement": "30334", "dateDebut": "2015-09-14",
			"etatAdministratifEtablissement": "A", "activitePrincipaleEtablissement": "86.90E",
			"nomenclatureActivitePrincipaleEtablissement": "NAFRev2", "caractereEmployeurEtablissement": "N",
			"activitePrincipaleNAF25Etablissement": "86.95Z",
		},
		{
			"siren": "333333333", "nic": "00019", "siret": "33333333300019", "statutDiffusionEtablissement": "O",
			"dateCreationEtablissement": "2018-02-20", "trancheEffectifsEtablissement": "11", "anneeEffectifsEtablissement": "2023",
			"dateDernierTraitementEtablissement": "2024-05-02 12:00:00", "etablissementSiege": "true",
			"nombrePeriodesEtablissement": "2", "numeroVoieEtablissement": "8", "typeVoieEtablissement": "PL",
			"libelleVoieEtablissement": "DE LA COMEDIE", "complementAdresseEtablissement": "BATIMENT A",
			"codePostalEtablissement": "34000", "libelleCommuneEtablissement": "MONTPELLIER",
			"codeCommuneEtablissement": "34172", "dateDebut": "2018-02-20", "etatAdministratifEtablissement": "A",
			"enseigne1Etablissement": "LA FOURNEE DE L'ETANG", "activitePrincipaleEtablissement": "10.71C",
			"nomenclatureActivitePrincipaleEtablissement": "NAFRev2", "caractereEmployeurEtablissement": "O",
			"activitePrincipaleNAF25Etablissement": "10.71C",
		},
		{
			"siren": "444444444", "nic": "00027", "siret": "44444444400027", "statutDiffusionEtablissement": "O",
			"dateCreationEtablissement": "2005-11-30", "dateDernierTraitementEtablissement": "2022-06-01 09:00:00",
			"etablissementSiege": "true", "nombrePeriodesEtablissement": "3", "numeroVoieEtablissement": "21",
			"typeVoieEtablissement": "COURS", "libelleVoieEtablissement": "NAPOLEON", "codePostalEtablissement": "20000",
			"libelleCommuneEtablissement": "AJACCIO", "codeCommuneEtablissement": "2A004", "dateDebut": "2020-12-31",
			"etatAdministratifEtablissement": "F", "activitePrincipaleEtablissement": "86.90E",
			"nomenclatureActivitePrincipaleEtablissement": "NAFRev2", "caractereEmployeurEtablissement": "N",
			"activitePrincipaleNAF25Etablissement": "86.95Z",
		},
		{
			"siren": "555555555", "nic": "00015", "siret": "55555555500015", "statutDiffusionEtablissement": "O",
			"dateCreationEtablissement": "2021-03-08", "trancheEffectifsEtablissement": "01", "anneeEffectifsEtablissement": "2023",
			"dateDernierTraitementEtablissement": "2024-02-20 14:00:00", "etablissementSiege": "true",
			"nombrePeriodesEtablissement": "1", "numeroVoieEtablissement": "5", "typeVoieEtablissement": "RUE",
			"libelleVoieEtablissement": "JULES VERNE", "codePostalEtablissement": "97400",
			"libelleCommuneEtablissement": "SAINT-DENIS", "codeCommuneEtablissement": "97411", "dateDebut": "2021-03-08",
			"etatAdministratifEtablissement": "A", "activitePrincipaleEtablissement": "86.90E",
			"nomenclatureActivitePrincipaleEtablissement": "NAFRev2", "caractereEmployeurEtablissement": "O",
			"activitePrincipaleNAF25Etablissement": "86.95Z",
		},
	}
	return ul, etab
}
