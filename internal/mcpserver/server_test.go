package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Noneo-Code/sirene-mcp/internal/sirenetest"
	"github.com/Noneo-Code/sirene-mcp/internal/store"
	"github.com/Noneo-Code/sirene-mcp/internal/syncer"
)

func fixtureStore(t *testing.T) *store.Store {
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

func connect(t *testing.T, s *store.Store) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := New(s, t.TempDir(), "test")
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestToolsListed(t *testing.T) {
	cs := connect(t, fixtureStore(t))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := "count_establishments,data_info,export_establishments,get_company,search_establishments,search_legal_categories,search_naf,sql_query"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("tools = %s\nwant    %s", got, want)
	}
}

func TestSearchToolKines(t *testing.T) {
	cs := connect(t, fixtureStore(t))
	res := call(t, cs, "search_establishments", map[string]any{"naf": []string{"8690E"}, "departements": []string{"30"}})
	if res.IsError {
		t.Fatalf("tool error: %s", text(res))
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out struct{ Total int64 }
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 2 {
		t.Errorf("total = %d, want 2 (%s)", out.Total, raw)
	}
}

func TestOtherToolsWork(t *testing.T) {
	cs := connect(t, fixtureStore(t))
	for name, args := range map[string]map[string]any{
		"count_establishments":    {"group_by": "departement"},
		"search_naf":              {"text": "kine"},
		"search_legal_categories": {"text": "sas"},
		"export_establishments":   {"format": "csv"},
		"sql_query":               {"query": "SELECT count(*) AS n FROM etablissement"},
		"data_info":               {},
	} {
		if res := call(t, cs, name, args); res.IsError {
			t.Errorf("%s: %s", name, text(res))
		}
	}
	if res := call(t, cs, "search_naf", map[string]any{"text": "kine", "version": "2025"}); res.IsError {
		t.Errorf("search_naf 2025: %s", text(res))
	}
	if res := call(t, cs, "search_naf", map[string]any{"text": "kine", "version": "x"}); !res.IsError {
		t.Error("bad version must be a tool error")
	}
}

func TestToolErrorsAreToolErrors(t *testing.T) {
	cs := connect(t, fixtureStore(t))
	res := call(t, cs, "get_company", map[string]any{"siren_or_siret": "abc"})
	if !res.IsError {
		t.Fatalf("expected IsError, got %s", text(res))
	}
}

func TestNoDatabaseMessage(t *testing.T) {
	cs := connect(t, store.Open(filepath.Join(t.TempDir(), "missing.duckdb")))
	for name, args := range map[string]map[string]any{
		"search_establishments": {},
		"data_info":             {},
		"sql_query":             {"query": "SELECT 1"},
	} {
		res := call(t, cs, name, args)
		if !res.IsError || !strings.Contains(text(res), "sirene-mcp sync") {
			t.Errorf("%s: IsError=%v text=%q", name, res.IsError, text(res))
		}
	}
}

func nomenclatureCall(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) EntriesOut {
	t.Helper()
	res := call(t, cs, tool, args)
	if res.IsError {
		t.Fatalf("%s: %s", tool, text(res))
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out EntriesOut
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"code":`) || !strings.Contains(string(raw), `"label":`) {
		t.Errorf("entries must use code/label keys: %.200s", raw)
	}
	if out.Total != len(out.Entries) {
		t.Errorf("total %d != %d entries", out.Total, len(out.Entries))
	}
	return out
}

func TestNomenclatureToolsNotTruncated(t *testing.T) {
	cs := connect(t, fixtureStore(t))
	if out := nomenclatureCall(t, cs, "search_naf", map[string]any{"text": ""}); out.Total != 732 {
		t.Errorf("search_naf empty: total %d, want 732", out.Total)
	}
	if out := nomenclatureCall(t, cs, "search_naf", map[string]any{"text": "", "version": "2025"}); out.Total < 700 {
		t.Errorf("search_naf 2025 empty: total %d", out.Total)
	}
	if out := nomenclatureCall(t, cs, "search_legal_categories", map[string]any{"text": ""}); out.Total != 260 {
		t.Errorf("search_legal_categories empty: total %d, want 260", out.Total)
	}
	if out := nomenclatureCall(t, cs, "search_naf", map[string]any{"text": "commerce"}); out.Total <= 50 {
		t.Errorf("search_naf commerce: total %d, want > 50", out.Total)
	}
	out := nomenclatureCall(t, cs, "search_naf", map[string]any{"text": "kiné"})
	if out.Total != 1 || out.Entries[0].Code != "86.90E" {
		t.Errorf("search_naf kiné: %+v", out)
	}
}

func TestSyncInProgressMessage(t *testing.T) {
	s := store.Open(filepath.Join(t.TempDir(), "missing.duckdb"))
	s.SetSyncing(true)
	cs := connect(t, s)
	res := call(t, cs, "search_establishments", map[string]any{})
	if !res.IsError || !strings.Contains(text(res), "sync in progress") {
		t.Errorf("IsError=%v text=%q", res.IsError, text(res))
	}
}
