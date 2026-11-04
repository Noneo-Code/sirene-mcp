package query

import (
	"slices"
	"strings"
	"testing"
)

func TestNormalizeNAF(t *testing.T) {
	for _, in := range []string{"8690E", "86.90e", " 86.90E "} {
		if got := normalizeNAF(in); got != "86.90E" {
			t.Errorf("normalizeNAF(%q) = %q", in, got)
		}
	}
}

func TestDepartementNormalization(t *testing.T) {
	for in, want := range map[string]string{"2a": "2A", "030": "30", "974": "974", " 30 ": "30"} {
		if got := normalizeDepartement(in); got != want {
			t.Errorf("normalizeDepartement(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWhereParameterized(t *testing.T) {
	clause, args, err := Filters{Name: "l'étang'; DROP TABLE x;--"}.Where()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(clause, "DROP") || strings.Contains(clause, "etang") {
		t.Errorf("input leaked into clause: %s", clause)
	}
	if !slices.Contains(args, any("l'etang'; drop table x;--")) {
		t.Errorf("args = %v", args)
	}
}

func TestBadDate(t *testing.T) {
	_, _, err := Filters{CreatedAfter: "2020-13-01"}.Where()
	if err == nil || !strings.Contains(err.Error(), "created_after") {
		t.Fatalf("err = %v", err)
	}
}

func TestBadEtat(t *testing.T) {
	if _, _, err := (Filters{Etat: "x"}).Where(); err == nil || !strings.Contains(err.Error(), "etat") {
		t.Fatalf("err = %v", err)
	}
}
