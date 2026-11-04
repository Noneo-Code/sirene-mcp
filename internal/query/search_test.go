package query

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Noneo-Code/sirene-mcp/internal/sirenetest"
	"github.com/Noneo-Code/sirene-mcp/internal/store"
	"github.com/Noneo-Code/sirene-mcp/internal/syncer"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	ul, etab := sirenetest.Default()
	dir := t.TempDir()
	ulPath, etabPath := sirenetest.Write(t, dir, ul, etab)
	out := filepath.Join(dir, "sirene.duckdb")
	if _, err := syncer.Build(context.Background(), syncer.BuildOptions{
		ULParquet: ulPath, EtabParquet: etabPath, OutPath: out,
		MemoryLimit: "1GB", TempDir: filepath.Join(dir, "tmp"),
	}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	s := store.Open(out)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSearchKinesGard(t *testing.T) {
	s := testStore(t)
	f := Filters{NAF: []string{"86.90E"}, Departements: []string{"30"}}
	r, err := Search(context.Background(), s, f, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 2 || len(r.Results) != 2 || r.Limit != 50 {
		t.Fatalf("got %+v", r)
	}
	var partial bool
	for _, e := range r.Results {
		if e.Commune == "" || e.NAFLibelle == "" || e.DateCreation == "" {
			t.Errorf("incomplete row %+v", e)
		}
		if e.Siren == "222222222" {
			partial = e.DiffusionPartielle
		} else if e.DiffusionPartielle {
			t.Errorf("unexpected partial %+v", e)
		}
	}
	if !partial {
		t.Error("Uzès row should be diffusion_partielle")
	}
	f.NAF = []string{"86.9"} // prefix
	if r, _ = Search(context.Background(), s, f, 0, 0); r.Total != 2 {
		t.Errorf("prefix total = %d", r.Total)
	}
}

func TestSearchIncludeClosed(t *testing.T) {
	s := testStore(t)
	r, err := Search(context.Background(), s, Filters{NAF: []string{"86.90E"}, Departements: []string{"30"}, Etat: "all"}, 0, 0)
	if err != nil || r.Total != 3 {
		t.Fatalf("total=%d err=%v", r.Total, err)
	}
}

func TestSearchByName(t *testing.T) {
	s := testStore(t)
	r, err := Search(context.Background(), s, Filters{Name: "etang"}, 0, 0)
	if err != nil || r.Total != 1 || r.Results[0].Siren != "333333333" {
		t.Fatalf("got %+v err=%v", r, err)
	}
	if r, _ = Search(context.Background(), s, Filters{Name: "Étang"}, 0, 0); r.Total != 1 {
		t.Errorf("accented name total = %d", r.Total)
	}
}

func TestSearchNameWildcardsAreLiteral(t *testing.T) {
	s := testStore(t)
	for _, name := range []string{"%", "_", "e%g"} {
		r, err := Search(context.Background(), s, Filters{Name: name, Etat: "all"}, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if r.Total != 0 {
			t.Errorf("name %q matched %d rows, want 0 (wildcards must be literal)", name, r.Total)
		}
	}
}

func TestSearchCapsButReportsTotal(t *testing.T) {
	s := testStore(t)
	r, err := Search(context.Background(), s, Filters{}, 2, 0)
	if err != nil || len(r.Results) != 2 || r.Total != 4 {
		t.Fatalf("got %+v err=%v", r, err)
	}
	if r, _ = Search(context.Background(), s, Filters{}, 10000, 0); r.Limit != 500 {
		t.Errorf("limit = %d", r.Limit)
	}
}

func TestSearchBadFilter(t *testing.T) {
	if _, err := Search(context.Background(), testStore(t), Filters{Etat: "zz"}, 0, 0); err == nil {
		t.Fatal("want error")
	}
}

func TestCountGroupByDepartement(t *testing.T) {
	s := testStore(t)
	r, err := Count(context.Background(), s, Filters{NAF: []string{"86.90E"}, Etat: "all"}, "departement")
	if err != nil {
		t.Fatal(err)
	}
	want := []Bucket{{"30", "", 3}, {"2A", "", 1}, {"974", "", 1}}
	if r.Total != 5 || len(r.Buckets) != 3 {
		t.Fatalf("got %+v", r)
	}
	for i, b := range want {
		if r.Buckets[i] != b {
			t.Errorf("bucket %d = %+v, want %+v", i, r.Buckets[i], b)
		}
	}
}

func TestCountLabelsAndErrors(t *testing.T) {
	s := testStore(t)
	r, err := Count(context.Background(), s, Filters{}, "naf")
	if err != nil || len(r.Buckets) == 0 || r.Buckets[0].Label == "" {
		t.Fatalf("got %+v err=%v", r, err)
	}
	if r, _ = Count(context.Background(), s, Filters{}, ""); r.Total != 4 || len(r.Buckets) != 0 {
		t.Errorf("plain count %+v", r)
	}
	if _, err = Count(context.Background(), s, Filters{}, "bogus"); err == nil {
		t.Error("want group_by error")
	}
}
