package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// stubTool is a Tool that records the args it received so registry tests
// can assert dispatch routing without pulling in real implementations.
type stubTool struct {
	name  string
	group string
	args  []byte
}

func (s *stubTool) Name() string  { return s.name }
func (s *stubTool) Group() string { return s.group }
func (s *stubTool) Schema() Schema {
	return Schema{Type: "function", Function: FunctionDecl{Name: s.name, Description: "stub"}}
}

func (s *stubTool) Dispatch(_ context.Context, _ *Env, raw json.RawMessage) Result {
	s.args = append([]byte(nil), raw...)
	return Result{Payload: map[string]interface{}{"called": s.name}}
}

func TestRegistryEnableExpandsGroups(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "list", group: "filesystem"})
	r.Register(&stubTool{name: "read", group: "filesystem"})
	r.Register(&stubTool{name: "find_my_cluster", group: "k8s"})

	enabled, err := r.Enable([]string{"filesystem", "k8s.find_my_cluster"})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	wantSorted := []string{"find_my_cluster", "list", "read"}
	if len(enabled) != len(wantSorted) {
		t.Fatalf("got %v, want %v", enabled, wantSorted)
	}
	for i, want := range wantSorted {
		if enabled[i] != want {
			t.Errorf("enabled[%d] = %q, want %q", i, enabled[i], want)
		}
	}
}

func TestRegistryEnableDedupes(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "list", group: "filesystem"})

	enabled, err := r.Enable([]string{"filesystem", "filesystem.list"})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if len(enabled) != 1 || enabled[0] != "list" {
		t.Errorf("expected dedup to [\"list\"], got %v", enabled)
	}
}

func TestRegistryEnableUnknownErrors(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "list", group: "filesystem"})

	if _, err := r.Enable([]string{"bogus"}); err == nil {
		t.Error("expected error for unknown group")
	}
	if _, err := r.Enable([]string{"filesystem.bogus"}); err == nil {
		t.Error("expected error for unknown tool")
	}
}

func TestRegistryDispatchUnknownReturnsErrPayload(t *testing.T) {
	r := NewRegistry()
	res := r.Dispatch(context.Background(), &Env{}, "no_such", nil)
	if res.Payload == nil || res.Payload["error"] == nil {
		t.Errorf("expected error payload, got %v", res.Payload)
	}
}

func TestRegistryDispatchPassesArgsToTool(t *testing.T) {
	r := NewRegistry()
	tool := &stubTool{name: "list", group: "filesystem"}
	r.Register(tool)
	raw := json.RawMessage(`{"path":"foo/"}`)
	r.Dispatch(context.Background(), &Env{}, "list", raw)
	if string(tool.args) != `{"path":"foo/"}` {
		t.Errorf("tool received args %q, want %q", tool.args, raw)
	}
}

func TestRegistryRegisterDuplicatePanics(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "list", group: "filesystem"})
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate tool name")
		}
	}()
	r.Register(&stubTool{name: "list", group: "filesystem"})
}

func TestRegistrySchemasSortedAndScoped(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "read", group: "filesystem"})
	r.Register(&stubTool{name: "list", group: "filesystem"})
	r.Register(&stubTool{name: "find_my_cluster", group: "k8s"})

	enabled, _ := r.Enable([]string{"filesystem"})
	got := r.Schemas(enabled)
	if len(got) != 2 {
		t.Fatalf("expected 2 schemas (filesystem only), got %d", len(got))
	}
	if got[0].Function.Name != "list" || got[1].Function.Name != "read" {
		t.Errorf("schemas not sorted: %s, %s", got[0].Function.Name, got[1].Function.Name)
	}
}

func TestCacheGetSetLen(t *testing.T) {
	c := NewCache()
	if c.Len() != 0 {
		t.Fatalf("empty cache Len = %d, want 0", c.Len())
	}
	if _, ok := c.Get("missing"); ok {
		t.Error("Get on missing key should report not-ok")
	}
	c.Set("k", "v1")
	c.Set("k", "v2")
	if got, ok := c.Get("k"); !ok || got != "v2" {
		t.Errorf("Get returned (%q, %v), want (\"v2\", true)", got, ok)
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d, want 1 after overwrite", c.Len())
	}
}

func TestCacheConcurrentSafe(t *testing.T) {
	c := NewCache()
	const writers, perWriter = 8, 100
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				key := string(rune('a'+w)) + string(rune('0'+i%10))
				c.Set(key, key)
				_, _ = c.Get(key)
			}
		}(w)
	}
	wg.Wait()
	// Keys collide intentionally, so only sanity-check for deadlock or panic.
	if c.Len() == 0 {
		t.Error("concurrent writers produced no entries")
	}
}

func TestBoundedCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewBoundedCache(2, 100)
	c.Set("a", "1")
	c.Set("b", "2")
	if _, ok := c.Get("a"); !ok {
		t.Fatal("expected a before eviction")
	}
	c.Set("c", "3")
	if _, ok := c.Get("b"); ok {
		t.Fatal("least recently used entry was retained")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

func TestBoundedCacheEnforcesByteLimit(t *testing.T) {
	c := NewBoundedCache(10, 5)
	c.Set("a", "12")
	c.Set("b", "34")
	if _, ok := c.Get("a"); ok {
		t.Fatal("oldest entry was retained past the byte limit")
	}
	if got := c.Bytes(); got > 5 {
		t.Fatalf("Bytes = %d, want <= 5", got)
	}
	c.Set("oversized", "value")
	if _, ok := c.Get("oversized"); ok {
		t.Fatal("oversized entry was cached")
	}
}

func TestErrPayloadEnvelope(t *testing.T) {
	res := ErrPayload("boom")
	if res.Payload["error"] != "boom" {
		t.Errorf("ErrPayload payload = %v, want {error:boom}", res.Payload)
	}
}

type catalogReader struct{}

func (catalogReader) ListTree(context.Context) ([]string, error) { return nil, nil }
func (catalogReader) ReadFile(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func TestSourceCatalogCanonicalOrderAndPrimary(t *testing.T) {
	catalog, err := NewSourceCatalog("project", []RepoSource{
		{ID: "server", Owner: "kubernetes", Name: "kubernetes", Revision: strings.Repeat("2", 40), Reader: catalogReader{}},
		{ID: "project", Owner: "example", Name: "project", Revision: strings.Repeat("1", 40), Reader: catalogReader{}},
		{ID: "client", Owner: "kubernetes", Name: "kubernetes", Revision: strings.Repeat("3", 40), Reader: catalogReader{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := catalog.Sources()
	if len(got) != 3 || got[0].ID != "client" || got[1].ID != "project" || got[2].ID != "server" {
		t.Fatalf("sources=%+v", got)
	}
	primary, ok := catalog.Primary()
	if !ok || primary.ID != "project" || catalog.PrimaryID() != "project" {
		t.Fatalf("primary=%+v ok=%t id=%q", primary, ok, catalog.PrimaryID())
	}
}

func TestSourceCatalogValidation(t *testing.T) {
	reader := catalogReader{}
	base := RepoSource{ID: "project", Owner: "example", Name: "project", Revision: strings.Repeat("1", 40), Reader: reader}
	cases := []struct {
		name    string
		primary string
		sources []RepoSource
	}{
		{name: "missing primary", primary: "other", sources: []RepoSource{base}},
		{name: "invalid id", primary: "Project", sources: []RepoSource{{ID: "Project", Owner: "example", Name: "project", Revision: strings.Repeat("1", 40), Reader: reader}}},
		{name: "duplicate id", primary: "project", sources: []RepoSource{base, {ID: "project", Owner: "example", Name: "other", Revision: strings.Repeat("2", 40), Reader: reader}}},
		{name: "duplicate revision", primary: "project", sources: []RepoSource{base, {ID: "mirror", Owner: "EXAMPLE", Name: "PROJECT", Revision: strings.Repeat("1", 40), Reader: reader}}},
		{name: "missing reader", primary: "project", sources: []RepoSource{{ID: "project", Owner: "example", Name: "project", Revision: strings.Repeat("1", 40)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSourceCatalog(tc.primary, tc.sources); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	tooMany := make([]RepoSource, maxRepoSources+1)
	for i := range tooMany {
		tooMany[i] = RepoSource{ID: "source-" + string(rune('a'+i)), Owner: "example", Name: "repo-" + string(rune('a'+i)), Revision: strings.Repeat("a", 40), Reader: reader}
	}
	if _, err := NewSourceCatalog(tooMany[0].ID, tooMany); err == nil {
		t.Fatal("expected source catalog bound error")
	}
}

func TestSourceCatalogAllowsSameRepositoryAtDifferentRevisions(t *testing.T) {
	catalog, err := NewSourceCatalog("client", []RepoSource{
		{ID: "client", Owner: "kubernetes", Name: "kubernetes", Revision: strings.Repeat("1", 40), Reader: catalogReader{}},
		{ID: "server", Owner: "kubernetes", Name: "kubernetes", Revision: strings.Repeat("2", 40), Reader: catalogReader{}},
	})
	if err != nil || len(catalog.Sources()) != 2 {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
}
