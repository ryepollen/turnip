package proc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatReference(t *testing.T) {
	tests := []struct {
		name string
		ref  Reference
		want string
	}{
		{"full", Reference{Name: "Дюна", Timecode: "01:23", Quote: "цитата"}, "«Дюна» [01:23] — «цитата»"},
		{"no quote", Reference{Name: "Дюна", Timecode: "01:23"}, "«Дюна» [01:23]"},
		{"name only", Reference{Name: "Дюна"}, "«Дюна»"},
		{"trims spaces", Reference{Name: "  Дюна  ", Quote: " q "}, "«Дюна» — «q»"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatReference(tt.ref))
		})
	}
}

func TestReferenceBlocks(t *testing.T) {
	refs := []Reference{
		{Type: "человек", Name: "Кант"},
		{Type: "книга", Name: "Дюна", Timecode: "01:00"},
		{Type: "weird", Name: "Штука"},    // → другое
		{Type: "книга", Name: "Гиперион"}, // second книга, same group
		{Type: "человек", Name: ""},       // empty name dropped
	}
	blocks := referenceBlocks(refs)

	// groups are emitted in referenceTypes order: книга (heading + 2 bullets),
	// человек (heading + 1 bullet), другое (heading + 1 bullet)
	types := make([]string, 0, len(blocks))
	for _, b := range blocks {
		types = append(types, b["type"].(string))
	}
	assert.Equal(t, []string{
		"heading_3", "bulleted_list_item", "bulleted_list_item",
		"heading_3", "bulleted_list_item",
		"heading_3", "bulleted_list_item",
	}, types)

	// first heading is the книга group
	h := blocks[0]["heading_3"].(map[string]any)["rich_text"].([]map[string]any)
	assert.Equal(t, "📚 Книга", h[0]["text"].(map[string]any)["content"])
}

func TestSplitRichText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want []string
	}{
		{"fits", "short", 10, []string{"short"}},
		{"split at space", "aaaa bbbb cccc", 10, []string{"aaaa bbbb ", "cccc"}},
		{"no space fallback", "aaaaaaaaaaaa", 5, []string{"aaaaa", "aaaaa", "aa"}},
		{"empty", "", 5, []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitRichText(tt.in, tt.max)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.in, strings.Join(got, ""), "lossless split")
		})
	}

	// cyrillic runes must not be broken: 2001 runes over the 2000 limit
	long := strings.Repeat("я", 2001)
	parts := splitRichText(long, 2000)
	require.Len(t, parts, 2)
	assert.Equal(t, long, strings.Join(parts, ""))
	for _, p := range parts {
		assert.True(t, len([]rune(p)) <= 2000)
	}
}

func TestMdToParagraphBlocks(t *testing.T) {
	md := "## Заголовок\n\nПервый абзац\nвторая строка.\n\n- пункт один\n- пункт два\n\nВторой абзац."
	blocks := mdToParagraphBlocks(md)
	require.Len(t, blocks, 5)
	assert.Equal(t, "heading_2", blocks[0]["type"])
	assert.Equal(t, "paragraph", blocks[1]["type"])
	assert.Equal(t, "bulleted_list_item", blocks[2]["type"])
	assert.Equal(t, "bulleted_list_item", blocks[3]["type"])
	assert.Equal(t, "paragraph", blocks[4]["type"])
}

func TestBatchBlocks(t *testing.T) {
	blocks := make([]map[string]any, 250)
	batches := batchBlocks(blocks, 100)
	require.Len(t, batches, 3)
	assert.Len(t, batches[0], 100)
	assert.Len(t, batches[2], 50)

	assert.Nil(t, batchBlocks(nil, 100))
}

func TestNormalizeRefType(t *testing.T) {
	assert.Equal(t, "книга", normalizeRefType(" Книга "))
	assert.Equal(t, "другое", normalizeRefType("book"))
	assert.Equal(t, "другое", normalizeRefType(""))
}

// memMetaStore is an in-memory NotionMetaStore for tests
type memMetaStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemMetaStore() *memMetaStore { return &memMetaStore{data: map[string][]byte{}} }

func (m *memMetaStore) SaveNotionMeta(key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = data
	return nil
}

func (m *memMetaStore) LoadNotionMeta(key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[key], nil
}

func (m *memMetaStore) DeleteNotionMetaPrefix(prefix string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			delete(m.data, k)
			n++
		}
	}
	return n, nil
}

// newNotionMock builds a Notion API mock covering bootstrap and episode writing
func newNotionMock(t *testing.T) (*httptest.Server, *struct {
	dbCreates, pageCreates, patches, dbRenames int
	maxBatch                                   int
	lastPage                                   map[string]any
	mu                                         sync.Mutex
}) {
	state := &struct {
		dbCreates, pageCreates, patches, dbRenames int
		maxBatch                                   int
		lastPage                                   map[string]any
		mu                                         sync.Mutex
	}{}

	mux := http.NewServeMux()
	mux.HandleFunc("/databases", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, notionVersion, r.Header.Get("Notion-Version"))
		state.mu.Lock()
		state.dbCreates++
		n := state.dbCreates
		state.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("db-%d", n)})
	})
	mux.HandleFunc("/databases/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/query") {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
			return
		}
		// PATCH /databases/{id} — the schema migration renames the db to Glossn
		if r.Method == http.MethodPatch {
			state.mu.Lock()
			state.dbRenames++
			state.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "db-1"})
			return
		}
		// databaseExists check for stale ids
		if strings.Contains(r.URL.Path, "stale") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"object":"error","status":404}`))
			return
		}
		// a database deleted in the Notion UI: 200 but archived
		if strings.Contains(r.URL.Path, "trashed") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "db-1", "archived": true, "in_trash": true})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "db-1"})
	})
	mux.HandleFunc("/pages", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		children, _ := body["children"].([]any)
		assert.LessOrEqual(t, len(children), 100, "initial children within limit")
		state.mu.Lock()
		state.pageCreates++
		state.lastPage = body
		n := state.pageCreates
		state.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id":  fmt.Sprintf("page-%d", n),
			"url": fmt.Sprintf("https://notion.so/page-%d", n),
		})
	})
	mux.HandleFunc("/pages/", func(w http.ResponseWriter, r *http.Request) {
		// pageAlive check for existing-page short-circuit
		_ = json.NewEncoder(w).Encode(map[string]any{"archived": false, "in_trash": false})
	})
	mux.HandleFunc("/blocks/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// the page carries two labelled toggles now; findToggleID matches by label
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
				{"id": "block-heading", "type": "heading_2"},
				{"id": "toggle-refs", "type": "toggle", "toggle": map[string]any{
					"rich_text": []map[string]any{{"plain_text": "📎 Отсылки (2)"}}}},
				{"id": "toggle-transcript", "type": "toggle", "toggle": map[string]any{
					"rich_text": []map[string]any{{"plain_text": "Транскрипт"}}}},
			}})
			return
		}
		var req struct {
			Children []map[string]any `json:"children"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.LessOrEqual(t, len(req.Children), 100, "append batch within limit")
		state.mu.Lock()
		state.patches++
		if len(req.Children) > state.maxBatch {
			state.maxBatch = len(req.Children)
		}
		state.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
	})

	return httptest.NewServer(mux), state
}

func newTestNotionWriter(baseURL string, meta NotionMetaStore) *NotionWriter {
	w := NewNotionWriter("test-token", "parent-page-id", meta)
	w.BaseURL = baseURL
	return w
}

func TestNotionEnsureDatabasesBootstrapAndReuse(t *testing.T) {
	ts, state := newNotionMock(t)
	defer ts.Close()

	store := newMemMetaStore()
	w := newTestNotionWriter(ts.URL, store)

	require.NoError(t, w.EnsureDatabases(context.Background()))
	assert.Equal(t, 1, state.dbCreates, "single Glossn database created")
	assert.NotEmpty(t, store.data[notionBootstrapKey])

	// a fresh writer with the same store must reuse persisted ids
	w2 := newTestNotionWriter(ts.URL, store)
	require.NoError(t, w2.EnsureDatabases(context.Background()))
	assert.Equal(t, 1, state.dbCreates, "no re-creation on second run")
	assert.Equal(t, 0, state.dbRenames, "current-schema bootstrap needs no migration")
}

func TestNotionEnsureDatabasesMigratesLegacyToGlossn(t *testing.T) {
	ts, state := newNotionMock(t)
	defer ts.Close()

	store := newMemMetaStore()
	// a schema-1 bootstrap (three databases, no Schema field) must be migrated
	// in place: the episodes db is renamed, no new db is created
	legacy, _ := json.Marshal(map[string]any{
		"parent_page_id": "parent-page-id",
		"episodes_db":    "legacy-episodes",
		"references_db":  "legacy-refs",
		"objects_db":     "legacy-objects",
	})
	require.NoError(t, store.SaveNotionMeta(notionBootstrapKey, legacy))

	w := newTestNotionWriter(ts.URL, store)
	require.NoError(t, w.EnsureDatabases(context.Background()))
	assert.Equal(t, 0, state.dbCreates, "legacy db reused, not recreated")
	assert.Equal(t, 1, state.dbRenames, "episodes db renamed to Glossn")

	// the persisted blob is now at the current schema — no second migration
	w2 := newTestNotionWriter(ts.URL, store)
	require.NoError(t, w2.EnsureDatabases(context.Background()))
	assert.Equal(t, 1, state.dbRenames, "migration runs once")
}

func TestNotionEnsureDatabasesRebootstrapOnStaleID(t *testing.T) {
	ts, state := newNotionMock(t)
	defer ts.Close()

	store := newMemMetaStore()
	stale, _ := json.Marshal(notionDBIDs{ParentPage: "parent-page-id", Episodes: "stale-id"})
	require.NoError(t, store.SaveNotionMeta(notionBootstrapKey, stale))

	w := newTestNotionWriter(ts.URL, store)
	require.NoError(t, w.EnsureDatabases(context.Background()))
	assert.Equal(t, 1, state.dbCreates, "stale id triggers re-bootstrap")
}

func TestNotionEnsureDatabasesRebootstrapOnTrashedDB(t *testing.T) {
	ts, state := newNotionMock(t)
	defer ts.Close()

	store := newMemMetaStore()
	// deleting a database in the Notion UI archives it: GET is 200 + archived
	trashed, _ := json.Marshal(notionDBIDs{ParentPage: "parent-page-id", Episodes: "trashed-id"})
	require.NoError(t, store.SaveNotionMeta(notionBootstrapKey, trashed))
	require.NoError(t, store.SaveNotionMeta("page:old-episode", []byte(`{"id":"x","url":"https://notion.so/dead"}`)))

	w := newTestNotionWriter(ts.URL, store)
	require.NoError(t, w.EnsureDatabases(context.Background()))
	assert.Equal(t, 1, state.dbCreates, "archived db triggers re-bootstrap")

	stale, err := store.LoadNotionMeta("page:old-episode")
	require.NoError(t, err)
	assert.Nil(t, stale, "stale page mappings dropped on re-bootstrap")
}

func TestNotionWriteEpisode(t *testing.T) {
	ts, state := newNotionMock(t)
	defer ts.Close()

	store := newMemMetaStore()
	w := newTestNotionWriter(ts.URL, store)

	// transcript long enough to require multiple 100-block batches
	var paras []string
	for i := 0; i < 250; i++ {
		paras = append(paras, fmt.Sprintf("[%02d:00] Абзац номер %d.", i, i))
	}

	in := EpisodeInput{
		Title: "Тестовый эпизод", URL: "https://youtu.be/x", Channel: "Канал",
		Date: "2026-06-15", DurationMin: 94, Tags: []string{"design"},
		Source: "youtube", CoverURL: "https://i.ytimg.com/vi/vid-1/hqdefault.jpg",
		Summary:    "Саммари.\n\n- мысль раз\n- мысль два",
		Transcript: strings.Join(paras, "\n\n"),
		Refs: []Reference{
			{Type: "книга", Name: "Дюна", Timecode: "01:00", Quote: "цитата"},
			{Type: "weird", Name: "Дюна", Timecode: "02:00", Quote: "дубль имени — тот же объект"},
		},
	}

	url, created, err := w.WriteEpisode(context.Background(), "vid-1", in)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "https://notion.so/page-1", url)

	// the episode page carries Тип=youtube and a thumbnail cover for the gallery/board
	props, _ := state.lastPage["properties"].(map[string]any)
	require.NotNil(t, props)
	typ, _ := props["Тип"].(map[string]any)
	sel, _ := typ["select"].(map[string]any)
	assert.Equal(t, "youtube", sel["name"], "Тип select set from source")
	cover, _ := state.lastPage["cover"].(map[string]any)
	ext, _ := cover["external"].(map[string]any)
	assert.Equal(t, "https://i.ytimg.com/vi/vid-1/hqdefault.jpg", ext["url"], "page cover = thumbnail")
	assert.GreaterOrEqual(t, state.patches, 3, "250 paragraphs (transcript) + refs need 3+ batches")
	assert.LessOrEqual(t, state.maxBatch, 100)
	// references now live inside the episode page, so only the episode page is created
	assert.Equal(t, 1, state.pageCreates)

	// second write of the same sourceID short-circuits to the existing page
	url2, created2, err := w.WriteEpisode(context.Background(), "vid-1", in)
	require.NoError(t, err)
	assert.False(t, created2)
	assert.Equal(t, url, url2)
	assert.Equal(t, 1, state.pageCreates, "no new pages")
}
