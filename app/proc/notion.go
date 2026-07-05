package proc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	notionAPIBase       = "https://api.notion.com/v1"
	notionVersion       = "2022-06-28"
	notionRichTextLimit = 2000 // max chars per rich_text item
	notionBlockBatch    = 100  // max blocks per append request
	notionCallInterval  = 350 * time.Millisecond
)

// notionBootstrapKey is the notion_meta key holding created database IDs
const notionBootstrapKey = "bootstrap"

// referenceTypes are the allowed values of the "Тип" select
var referenceTypes = []string{
	"книга", "фильм", "сериал", "человек", "инструмент",
	"статья", "компания", "подкаст", "концепция", "другое",
}

// NotionMetaStore persists opaque Notion metadata (implemented by ytstore.BoltDB)
type NotionMetaStore interface {
	SaveNotionMeta(key string, data []byte) error
	LoadNotionMeta(key string) ([]byte, error)
	DeleteNotionMetaPrefix(prefix string) (int, error)
}

// notionDBIDs is the persisted bootstrap state
type notionDBIDs struct {
	ParentPage string    `json:"parent_page_id"`
	Episodes   string    `json:"episodes_db"`
	References string    `json:"references_db"`
	Objects    string    `json:"objects_db"`
	Digests    string    `json:"digests_db,omitempty"` // created lazily on first /digest
	CreatedAt  time.Time `json:"created_at"`
}

// notionPageRef is the persisted mapping sourceID -> episode page
type notionPageRef struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// EpisodeInput is everything needed to publish one episode to Notion
type EpisodeInput struct {
	Title       string
	URL         string
	Channel     string
	Date        string // YYYY-MM-DD
	DurationMin int
	Tags        []string
	Summary     string
	Transcript  string
	Refs        []Reference
}

// NotionWriter publishes episode notes into three Notion databases
// (Эпизоды, Отсылки, Объекты) created under a configured parent page.
type NotionWriter struct {
	Token      string
	ParentPage string
	BaseURL    string
	Meta       NotionMetaStore
	client     *http.Client

	mu  sync.Mutex // guards bootstrap
	ids notionDBIDs

	thMu     sync.Mutex // guards throttle state
	lastCall time.Time
}

// NewNotionWriter creates a Notion publisher
func NewNotionWriter(token, parentPage string, meta NotionMetaStore) *NotionWriter {
	return &NotionWriter{
		Token:      token,
		ParentPage: parentPage,
		BaseURL:    notionAPIBase,
		Meta:       meta,
		client:     &http.Client{Timeout: 60 * time.Second},
	}
}

// EnsureDatabases creates the three databases under ParentPage if they do not
// exist yet. Idempotent: IDs are persisted via Meta and revalidated on load.
func (w *NotionWriter) EnsureDatabases(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.ids.Episodes != "" && w.ids.ParentPage == w.ParentPage {
		return nil
	}

	if data, err := w.Meta.LoadNotionMeta(notionBootstrapKey); err == nil && len(data) > 0 {
		var stored notionDBIDs
		if jerr := json.Unmarshal(data, &stored); jerr == nil &&
			stored.ParentPage == w.ParentPage && w.databaseExists(ctx, stored.Episodes) {
			w.ids = stored
			return nil
		}
		log.Printf("[INFO] stored notion databases stale or parent changed, re-bootstrapping")
	}

	log.Printf("[INFO] creating notion databases under page %s", w.ParentPage)
	// old page mappings point into deleted/stale databases — drop them so
	// repeated /notes re-creates pages instead of returning dead links
	if n, derr := w.Meta.DeleteNotionMetaPrefix("page:"); derr != nil {
		log.Printf("[WARN] failed to drop stale notion page mappings: %v", derr)
	} else if n > 0 {
		log.Printf("[INFO] dropped %d stale notion page mappings", n)
	}
	ids := notionDBIDs{ParentPage: w.ParentPage, CreatedAt: time.Now()}

	episodesID, err := w.createDatabase(ctx, "Эпизоды", map[string]any{
		"Name":               map[string]any{"title": map[string]any{}},
		"URL":                map[string]any{"url": map[string]any{}},
		"Канал":              map[string]any{"rich_text": map[string]any{}},
		"Дата":               map[string]any{"date": map[string]any{}},
		"Длительность (мин)": map[string]any{"number": map[string]any{}},
		"Теги":               map[string]any{"multi_select": map[string]any{}},
	})
	if err != nil {
		return fmt.Errorf("failed to create episodes db: %w", err)
	}
	ids.Episodes = episodesID

	objectsID, err := w.createDatabase(ctx, "Объекты", map[string]any{
		"Название": map[string]any{"title": map[string]any{}},
		"Тип":      map[string]any{"select": selectOptions()},
	})
	if err != nil {
		return fmt.Errorf("failed to create objects db: %w", err)
	}
	ids.Objects = objectsID

	// two-way relations: the episode page shows its «Отсылки» right in the
	// properties, the object page shows all its mentions
	referencesID, err := w.createDatabase(ctx, "Отсылки", map[string]any{
		"Название": map[string]any{"title": map[string]any{}},
		"Тип":      map[string]any{"select": selectOptions()},
		"Таймкод":  map[string]any{"rich_text": map[string]any{}},
		"Цитата":   map[string]any{"rich_text": map[string]any{}},
		"Эпизод": map[string]any{"relation": map[string]any{"database_id": episodesID,
			"dual_property": map[string]any{"synced_property_name": "Отсылки"}}},
		"Объект": map[string]any{"relation": map[string]any{"database_id": objectsID,
			"dual_property": map[string]any{"synced_property_name": "Упоминания"}}},
	})
	if err != nil {
		return fmt.Errorf("failed to create references db: %w", err)
	}
	ids.References = referencesID

	data, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("failed to marshal db ids: %w", err)
	}
	if err := w.Meta.SaveNotionMeta(notionBootstrapKey, data); err != nil {
		return fmt.Errorf("failed to persist db ids: %w", err)
	}
	w.ids = ids
	return nil
}

// WriteEpisode publishes one episode: page with summary + transcript toggle,
// deduplicated objects and reference rows. If the episode page already exists
// (by sourceID) and is still alive, returns its URL without rewriting.
// EnsureDatabases runs BEFORE the mapping check: a re-bootstrap (databases
// deleted in the Notion UI) drops stale page mappings, otherwise the early
// return would hand out links into deleted databases forever.
func (w *NotionWriter) WriteEpisode(ctx context.Context, sourceID string, in EpisodeInput) (pageURL string, created bool, err error) {
	if err := w.EnsureDatabases(ctx); err != nil {
		return "", false, err
	}

	if data, lerr := w.Meta.LoadNotionMeta("page:" + sourceID); lerr == nil && len(data) > 0 {
		var ref notionPageRef
		if jerr := json.Unmarshal(data, &ref); jerr == nil && ref.URL != "" {
			if w.pageAlive(ctx, ref.ID) {
				return ref.URL, false, nil
			}
			log.Printf("[INFO] episode page for %s is gone, recreating", sourceID)
		}
	}

	pageID, pageURL, err := w.createEpisodePage(ctx, in)
	if err != nil {
		if strings.Contains(err.Error(), "object_not_found") {
			w.invalidateIDs() // databases vanished mid-flight, revalidate next run
		}
		return "", false, fmt.Errorf("failed to create episode page: %w", err)
	}

	if err := w.appendTranscript(ctx, pageID, in.Transcript); err != nil {
		return pageURL, true, fmt.Errorf("failed to append transcript: %w", err)
	}

	if err := w.writeReferences(ctx, pageID, in.Refs); err != nil {
		return pageURL, true, fmt.Errorf("failed to write references: %w", err)
	}

	if data, jerr := json.Marshal(notionPageRef{ID: pageID, URL: pageURL}); jerr == nil {
		if serr := w.Meta.SaveNotionMeta("page:"+sourceID, data); serr != nil {
			log.Printf("[WARN] failed to persist notion page mapping for %s: %v", sourceID, serr)
		}
	}
	return pageURL, true, nil
}

// ensureDigestDatabase lazily creates the «Дайджесты» database (second table
// on the hub page, below Эпизоды) and persists its id in the bootstrap blob
func (w *NotionWriter) ensureDigestDatabase(ctx context.Context) error {
	if err := w.EnsureDatabases(ctx); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ids.Digests != "" && w.databaseExists(ctx, w.ids.Digests) {
		return nil
	}

	digestsID, err := w.createDatabase(ctx, "Дайджесты", map[string]any{
		"Тема":            map[string]any{"title": map[string]any{}},
		"Тег":             map[string]any{"select": map[string]any{"options": []map[string]any{}}},
		"Дата пересборки": map[string]any{"date": map[string]any{}},
		"Эпизоды": map[string]any{"relation": map[string]any{"database_id": w.ids.Episodes,
			"dual_property": map[string]any{"synced_property_name": "Дайджесты"}}},
	})
	if err != nil {
		return fmt.Errorf("failed to create digests db: %w", err)
	}
	w.ids.Digests = digestsID

	data, err := json.Marshal(w.ids)
	if err != nil {
		return fmt.Errorf("failed to marshal db ids: %w", err)
	}
	if err := w.Meta.SaveNotionMeta(notionBootstrapKey, data); err != nil {
		return fmt.Errorf("failed to persist db ids: %w", err)
	}
	return nil
}

// EpisodePageID returns the Notion page id for a sourceID, "" if the episode
// never went through /notes
func (w *NotionWriter) EpisodePageID(sourceID string) string {
	data, err := w.Meta.LoadNotionMeta("page:" + sourceID)
	if err != nil || len(data) == 0 {
		return ""
	}
	var ref notionPageRef
	if json.Unmarshal(data, &ref) != nil {
		return ""
	}
	return ref.ID
}

// ExistingDigestURL returns the current digest page URL for a tag, "" if none
func (w *NotionWriter) ExistingDigestURL(tag string) string {
	data, err := w.Meta.LoadNotionMeta("digest:" + tag)
	if err != nil || len(data) == 0 {
		return ""
	}
	var ref notionPageRef
	if json.Unmarshal(data, &ref) != nil {
		return ""
	}
	return ref.URL
}

// WriteDigest publishes a rebuilt digest: a fresh page in «Дайджесты» with
// relations to episode pages; the previous version of this tag's digest is
// archived so the database always holds one current page per topic
func (w *NotionWriter) WriteDigest(ctx context.Context, tag, title, body string, episodePages []string) (pageURL string, err error) {
	if err := w.ensureDigestDatabase(ctx); err != nil {
		return "", err
	}

	props := map[string]any{
		"Тема":            map[string]any{"title": richText(title)},
		"Тег":             map[string]any{"select": map[string]any{"name": tag}},
		"Дата пересборки": map[string]any{"date": map[string]any{"start": time.Now().Format("2006-01-02")}},
	}
	if len(episodePages) > 0 {
		rel := make([]map[string]any, 0, len(episodePages))
		for _, id := range episodePages {
			rel = append(rel, map[string]any{"id": id})
		}
		props["Эпизоды"] = map[string]any{"relation": rel}
	}

	var resp struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := w.doNotion(ctx, "POST", "/pages", map[string]any{
		"parent":     map[string]any{"database_id": w.ids.Digests},
		"properties": props,
	}, &resp); err != nil {
		if strings.Contains(err.Error(), "object_not_found") {
			w.invalidateIDs()
		}
		return "", fmt.Errorf("failed to create digest page: %w", err)
	}

	for _, batch := range batchBlocks(mdToParagraphBlocks(body), notionBlockBatch) {
		if err := w.doNotion(ctx, "PATCH", "/blocks/"+resp.ID+"/children",
			map[string]any{"children": batch}, nil); err != nil {
			return resp.URL, fmt.Errorf("failed to append digest body: %w", err)
		}
	}

	// archive the previous version, keep exactly one live page per tag
	if data, lerr := w.Meta.LoadNotionMeta("digest:" + tag); lerr == nil && len(data) > 0 {
		var prev notionPageRef
		if json.Unmarshal(data, &prev) == nil && prev.ID != "" {
			if aerr := w.doNotion(ctx, "PATCH", "/pages/"+prev.ID,
				map[string]any{"archived": true}, nil); aerr != nil {
				log.Printf("[WARN] failed to archive previous digest %s: %v", prev.ID, aerr)
			}
		}
	}
	if data, jerr := json.Marshal(notionPageRef{ID: resp.ID, URL: resp.URL}); jerr == nil {
		if serr := w.Meta.SaveNotionMeta("digest:"+tag, data); serr != nil {
			log.Printf("[WARN] failed to persist digest mapping for %s: %v", tag, serr)
		}
	}
	return resp.URL, nil
}

// createEpisodePage creates the page with properties, summary and an empty
// transcript toggle (filled separately due to the 100-blocks-per-request limit)
func (w *NotionWriter) createEpisodePage(ctx context.Context, in EpisodeInput) (pageID, pageURL string, err error) {
	props := map[string]any{
		"Name": map[string]any{"title": richText(in.Title)},
		"Теги": map[string]any{"multi_select": multiSelect(in.Tags)},
	}
	if in.URL != "" {
		props["URL"] = map[string]any{"url": in.URL}
	}
	if in.Channel != "" {
		props["Канал"] = map[string]any{"rich_text": richText(in.Channel)}
	}
	if in.Date != "" {
		props["Дата"] = map[string]any{"date": map[string]any{"start": in.Date}}
	}
	if in.DurationMin > 0 {
		props["Длительность (мин)"] = map[string]any{"number": in.DurationMin}
	}

	children := []map[string]any{heading2Block("Саммари")}
	summaryBlocks := mdToParagraphBlocks(in.Summary)
	if len(summaryBlocks) > notionBlockBatch-2 { // heading + toggle must fit too
		summaryBlocks = summaryBlocks[:notionBlockBatch-2]
	}
	children = append(children, summaryBlocks...)
	children = append(children, map[string]any{
		"object": "block",
		"type":   "toggle",
		"toggle": map[string]any{"rich_text": richText("Транскрипт")},
	})

	var resp struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	err = w.doNotion(ctx, "POST", "/pages", map[string]any{
		"parent":     map[string]any{"database_id": w.ids.Episodes},
		"properties": props,
		"children":   children,
	}, &resp)
	if err != nil {
		return "", "", err
	}
	return resp.ID, resp.URL, nil
}

// appendTranscript finds the transcript toggle on the page and fills it in
// batches of at most 100 blocks
func (w *NotionWriter) appendTranscript(ctx context.Context, pageID, transcript string) error {
	var list struct {
		Results []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"results"`
	}
	if err := w.doNotion(ctx, "GET", "/blocks/"+pageID+"/children?page_size=100", nil, &list); err != nil {
		return fmt.Errorf("failed to list page children: %w", err)
	}
	toggleID := ""
	for _, b := range list.Results {
		if b.Type == "toggle" {
			toggleID = b.ID // the page has a single toggle, created by us
		}
	}
	if toggleID == "" {
		return fmt.Errorf("transcript toggle not found on page %s", pageID)
	}

	for _, batch := range batchBlocks(mdToParagraphBlocks(transcript), notionBlockBatch) {
		if err := w.doNotion(ctx, "PATCH", "/blocks/"+toggleID+"/children",
			map[string]any{"children": batch}, nil); err != nil {
			return err
		}
	}
	return nil
}

// writeReferences creates deduplicated object pages and one reference row per mention
func (w *NotionWriter) writeReferences(ctx context.Context, episodePageID string, refs []Reference) error {
	objectCache := map[string]string{} // lowercased name -> page id
	for _, ref := range refs {
		name := strings.TrimSpace(ref.Name)
		if name == "" {
			continue
		}
		objID, err := w.ensureObject(ctx, name, normalizeRefType(ref.Type), objectCache)
		if err != nil {
			return fmt.Errorf("failed to ensure object %q: %w", name, err)
		}

		props := map[string]any{
			"Название": map[string]any{"title": richText(name)},
			"Тип":      map[string]any{"select": map[string]any{"name": normalizeRefType(ref.Type)}},
			"Эпизод":   map[string]any{"relation": []map[string]any{{"id": episodePageID}}},
			"Объект":   map[string]any{"relation": []map[string]any{{"id": objID}}},
		}
		if ref.Timecode != "" {
			props["Таймкод"] = map[string]any{"rich_text": richText(ref.Timecode)}
		}
		if ref.Quote != "" {
			props["Цитата"] = map[string]any{"rich_text": richText(ref.Quote)}
		}
		if err := w.doNotion(ctx, "POST", "/pages", map[string]any{
			"parent":     map[string]any{"database_id": w.ids.References},
			"properties": props,
		}, nil); err != nil {
			return fmt.Errorf("failed to create reference %q: %w", name, err)
		}
	}
	return nil
}

// ensureObject returns the page id for an entity, creating it if absent
func (w *NotionWriter) ensureObject(ctx context.Context, name, refType string, cache map[string]string) (string, error) {
	key := strings.ToLower(name)
	if id, ok := cache[key]; ok {
		return id, nil
	}

	var query struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	err := w.doNotion(ctx, "POST", "/databases/"+w.ids.Objects+"/query", map[string]any{
		"filter": map[string]any{"property": "Название", "title": map[string]any{"equals": name}},
	}, &query)
	if err != nil {
		return "", err
	}
	if len(query.Results) > 0 {
		cache[key] = query.Results[0].ID
		return query.Results[0].ID, nil
	}

	var created struct {
		ID string `json:"id"`
	}
	err = w.doNotion(ctx, "POST", "/pages", map[string]any{
		"parent": map[string]any{"database_id": w.ids.Objects},
		"properties": map[string]any{
			"Название": map[string]any{"title": richText(name)},
			"Тип":      map[string]any{"select": map[string]any{"name": refType}},
		},
	}, &created)
	if err != nil {
		return "", err
	}
	cache[key] = created.ID
	return created.ID, nil
}

// createDatabase creates one database under the parent page and returns its id
func (w *NotionWriter) createDatabase(ctx context.Context, title string, properties map[string]any) (string, error) {
	var resp struct {
		ID string `json:"id"`
	}
	err := w.doNotion(ctx, "POST", "/databases", map[string]any{
		"parent":     map[string]any{"type": "page_id", "page_id": w.ParentPage},
		"title":      richText(title),
		"properties": properties,
	}, &resp)
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// pageAlive checks that a stored page id still points to a live page
// (deleting a page in the Notion UI archives it, deleting its database 404s)
func (w *NotionWriter) pageAlive(ctx context.Context, id string) bool {
	if id == "" {
		return false
	}
	var page struct {
		Archived bool `json:"archived"`
		InTrash  bool `json:"in_trash"`
	}
	if err := w.doNotion(ctx, "GET", "/pages/"+id, nil, &page); err != nil {
		return false
	}
	return !page.Archived && !page.InTrash
}

// databaseExists checks that a stored database id points to a live database.
// Deleting a database in the Notion UI archives it (GET still returns 200
// with archived/in_trash set), so the flags matter as much as the status.
func (w *NotionWriter) databaseExists(ctx context.Context, id string) bool {
	if id == "" {
		return false
	}
	var db struct {
		Archived bool `json:"archived"`
		InTrash  bool `json:"in_trash"`
	}
	if err := w.doNotion(ctx, "GET", "/databases/"+id, nil, &db); err != nil {
		return false
	}
	return !db.Archived && !db.InTrash
}

// invalidateIDs drops the in-memory bootstrap cache: the next call revalidates
// the stored databases and re-bootstraps if they are gone
func (w *NotionWriter) invalidateIDs() {
	w.mu.Lock()
	w.ids = notionDBIDs{}
	w.mu.Unlock()
}

// doNotion makes one throttled Notion API call with retries
func (w *NotionWriter) doNotion(ctx context.Context, method, path string, body, out any) error {
	var jsonData []byte
	if body != nil {
		var err error
		if jsonData, err = json.Marshal(body); err != nil {
			return fmt.Errorf("failed to marshal request: %w", err)
		}
	}

	w.throttle()

	build := func() (*http.Request, error) {
		var rdr *bytes.Reader
		if jsonData != nil {
			rdr = bytes.NewReader(jsonData)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, err := http.NewRequestWithContext(ctx, method, w.BaseURL+path, rdr)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+w.Token)
		req.Header.Set("Notion-Version", notionVersion)
		if jsonData != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	}

	resp, err := doWithRetry(ctx, w.client, build)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("failed to decode response: %w", err)
		}
	}
	return nil
}

// throttle keeps calls at least notionCallInterval apart (Notion allows ~3 rps)
func (w *NotionWriter) throttle() {
	w.thMu.Lock()
	defer w.thMu.Unlock()
	if elapsed := time.Since(w.lastCall); elapsed < notionCallInterval {
		time.Sleep(notionCallInterval - elapsed)
	}
	w.lastCall = time.Now()
}

// normalizeRefType maps an arbitrary LLM-produced type to an allowed select option
func normalizeRefType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	for _, allowed := range referenceTypes {
		if t == allowed {
			return t
		}
	}
	return "другое"
}

// selectOptions builds the select schema shared by Отсылки and Объекты
func selectOptions() map[string]any {
	opts := make([]map[string]any, 0, len(referenceTypes))
	for _, t := range referenceTypes {
		opts = append(opts, map[string]any{"name": t})
	}
	return map[string]any{"options": opts}
}

// richText builds a rich_text array, splitting content at the 2000-char limit
func richText(s string) []map[string]any {
	parts := splitRichText(s, notionRichTextLimit)
	res := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		res = append(res, map[string]any{"type": "text", "text": map[string]any{"content": p}})
	}
	return res
}

// splitRichText splits s into pieces of at most max chars, preferring word
// boundaries and never splitting runes
func splitRichText(s string, max int) []string {
	runes := []rune(s)
	if len(runes) <= max {
		return []string{s}
	}
	var parts []string
	for len(runes) > max {
		cut := max
		for i := max; i > max/2; i-- {
			if runes[i-1] == ' ' || runes[i-1] == '\n' {
				cut = i
				break
			}
		}
		parts = append(parts, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		parts = append(parts, string(runes))
	}
	return parts
}

// mdToParagraphBlocks converts markdown-ish text to Notion blocks: headings,
// bullets and paragraphs. Blank-line separated chunks become individual blocks.
func mdToParagraphBlocks(md string) []map[string]any {
	var blocks []map[string]any
	for _, para := range strings.Split(md, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		// a chunk may hold consecutive bullet lines — keep them as separate blocks
		lines := strings.Split(para, "\n")
		var plain []string
		flushPlain := func() {
			if len(plain) > 0 {
				blocks = append(blocks, paragraphBlock(strings.Join(plain, "\n")))
				plain = nil
			}
		}
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "## "):
				flushPlain()
				blocks = append(blocks, heading2Block(strings.TrimPrefix(trimmed, "## ")))
			case strings.HasPrefix(trimmed, "# "):
				flushPlain()
				blocks = append(blocks, heading2Block(strings.TrimPrefix(trimmed, "# ")))
			case strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "• ") || strings.HasPrefix(trimmed, "* "):
				flushPlain()
				blocks = append(blocks, map[string]any{
					"object":             "block",
					"type":               "bulleted_list_item",
					"bulleted_list_item": map[string]any{"rich_text": richText(trimmed[strings.IndexRune(trimmed, ' ')+1:])},
				})
			default:
				plain = append(plain, line)
			}
		}
		flushPlain()
	}
	return blocks
}

// batchBlocks splits blocks into batches of at most n
func batchBlocks(blocks []map[string]any, n int) [][]map[string]any {
	var batches [][]map[string]any
	for len(blocks) > n {
		batches = append(batches, blocks[:n])
		blocks = blocks[n:]
	}
	if len(blocks) > 0 {
		batches = append(batches, blocks)
	}
	return batches
}

func paragraphBlock(text string) map[string]any {
	return map[string]any{
		"object":    "block",
		"type":      "paragraph",
		"paragraph": map[string]any{"rich_text": richText(text)},
	}
}

func heading2Block(text string) map[string]any {
	return map[string]any{
		"object":    "block",
		"type":      "heading_2",
		"heading_2": map[string]any{"rich_text": richText(text)},
	}
}

// multiSelect builds a multi_select property value
func multiSelect(tags []string) []map[string]any {
	res := make([]map[string]any, 0, len(tags))
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			res = append(res, map[string]any{"name": t})
		}
	}
	return res
}
