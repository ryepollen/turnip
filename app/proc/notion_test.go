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

// newNotionMock builds a Notion API mock covering bootstrap and episode writing
func newNotionMock(t *testing.T) (*httptest.Server, *struct {
	dbCreates, pageCreates, patches int
	maxBatch                        int
	mu                              sync.Mutex
}) {
	state := &struct {
		dbCreates, pageCreates, patches int
		maxBatch                        int
		mu                              sync.Mutex
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
		// databaseExists check for stale ids
		if strings.Contains(r.URL.Path, "stale") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"object":"error","status":404}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "db-1"})
	})
	mux.HandleFunc("/pages", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Children []map[string]any `json:"children"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		assert.LessOrEqual(t, len(req.Children), 100, "initial children within limit")
		state.mu.Lock()
		state.pageCreates++
		n := state.pageCreates
		state.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id":  fmt.Sprintf("page-%d", n),
			"url": fmt.Sprintf("https://notion.so/page-%d", n),
		})
	})
	mux.HandleFunc("/blocks/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]string{
				{"id": "block-heading", "type": "heading_2"},
				{"id": "block-toggle", "type": "toggle"},
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
	assert.Equal(t, 3, state.dbCreates, "three databases created")
	assert.NotEmpty(t, store.data[notionBootstrapKey])

	// a fresh writer with the same store must reuse persisted ids
	w2 := newTestNotionWriter(ts.URL, store)
	require.NoError(t, w2.EnsureDatabases(context.Background()))
	assert.Equal(t, 3, state.dbCreates, "no re-creation on second run")
}

func TestNotionEnsureDatabasesRebootstrapOnStaleID(t *testing.T) {
	ts, state := newNotionMock(t)
	defer ts.Close()

	store := newMemMetaStore()
	stale, _ := json.Marshal(notionDBIDs{ParentPage: "parent-page-id", Episodes: "stale-id"})
	require.NoError(t, store.SaveNotionMeta(notionBootstrapKey, stale))

	w := newTestNotionWriter(ts.URL, store)
	require.NoError(t, w.EnsureDatabases(context.Background()))
	assert.Equal(t, 3, state.dbCreates, "stale id triggers re-bootstrap")
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
	assert.GreaterOrEqual(t, state.patches, 3, "250 paragraphs need 3+ batches")
	assert.LessOrEqual(t, state.maxBatch, 100)
	// pages: 1 episode + 1 object (deduped) + 2 references
	assert.Equal(t, 4, state.pageCreates)

	// second write of the same sourceID short-circuits to the existing page
	url2, created2, err := w.WriteEpisode(context.Background(), "vid-1", in)
	require.NoError(t, err)
	assert.False(t, created2)
	assert.Equal(t, url, url2)
	assert.Equal(t, 4, state.pageCreates, "no new pages")
}
