package query

import (
	"context"
	"database/sql"
	"fmt"

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
		q := `SELECT e.siret, e.siren, coalesce(e.nom_affiche,''), coalesce(e.activite_principale,''), coalesce(n.libelle,''),
  coalesce(e.activite_principale_naf25,''), coalesce(e.adresse_complete,''), coalesce(e.code_postal,''),
  coalesce(e.libelle_commune,''), coalesce(e.departement,''), coalesce(e.tranche_effectifs,''), coalesce(t.libelle,''),
  coalesce(e.categorie_juridique,''), coalesce(CAST(e.date_creation AS VARCHAR),''), coalesce(e.etat_administratif,''),
  coalesce(e.est_siege,false), coalesce(e.statut_diffusion,'') = 'P'
FROM etablissement e
LEFT JOIN naf n ON n.code = e.activite_principale
LEFT JOIN tranche_effectifs t ON t.code = e.tranche_effectifs
WHERE ` + where + `
ORDER BY e.departement, e.code_postal, e.nom_affiche, e.siret
LIMIT ? OFFSET ?`
		rows, err := db.QueryContext(ctx, q, append(append([]any{}, args...), limit, offset)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Establishment
			if err := rows.Scan(&e.Siret, &e.Siren, &e.NomAffiche, &e.NAF, &e.NAFLibelle, &e.NAF25, &e.AdresseComplete,
				&e.CodePostal, &e.Commune, &e.Departement, &e.TrancheEffectifs, &e.TrancheLibelle,
				&e.CategorieJuridique, &e.DateCreation, &e.Etat, &e.EstSiege, &e.DiffusionPartielle); err != nil {
				return err
			}
			res.Results = append(res.Results, e)
		}
		return rows.Err()
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
