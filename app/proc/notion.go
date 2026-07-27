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
	"unicode"
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

// currentNotionSchema is bumped when the database layout changes. Schema 1 was
// three databases (Эпизоды/Отсылки/Объекты); schema 2 collapses them into a
// single «Glossn» database with references living inside each episode page.
// A stored bootstrap at a lower schema is migrated in place (the episodes db is
// renamed to Glossn) so existing episode pages are preserved.
const currentNotionSchema = 2

// glossnDBTitle is the display name of the single episodes database
const glossnDBTitle = "Glossn"

// referenceTypes are the reference categories, in the order they are grouped in
// the «📎 Отсылки» toggle. Also the allowed values normalizeRefType maps to.
var referenceTypes = []string{
	"книга", "фильм", "сериал", "человек", "инструмент",
	"статья", "компания", "подкаст", "концепция", "другое",
}

// refTypeEmoji is the bullet-group marker per reference type
var refTypeEmoji = map[string]string{
	"книга": "📚", "фильм": "🎬", "сериал": "📺", "человек": "👤",
	"инструмент": "🛠", "статья": "📄", "компания": "🏢",
	"подкаст": "🎙", "концепция": "💡", "другое": "📌",
}

// NotionMetaStore persists opaque Notion metadata (implemented by ytstore.BoltDB)
type NotionMetaStore interface {
	SaveNotionMeta(key string, data []byte) error
	LoadNotionMeta(key string) ([]byte, error)
	DeleteNotionMetaPrefix(prefix string) (int, error)
}

// notionDBIDs is the persisted bootstrap state. Since schema 2 there is a single
// episodes database (Episodes, titled «Glossn»); the legacy References/Objects
// ids are gone. Schema records the layout version for in-place migration.
type notionDBIDs struct {
	ParentPage string    `json:"parent_page_id"`
	Episodes   string    `json:"episodes_db"`          // the «Glossn» database
	Digests    string    `json:"digests_db,omitempty"` // created lazily on first /digest
	Schema     int       `json:"schema,omitempty"`
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

// NotionWriter publishes episode notes into a single Notion database («Glossn»)
// created under a configured parent page. References live inside each episode
// page under a «📎 Отсылки» toggle, not as separate database rows.
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

// EnsureDatabases creates the single «Glossn» database under ParentPage if it
// does not exist yet. Idempotent: the id is persisted via Meta and revalidated
// on load. A stored bootstrap from an older schema is migrated in place (the
// legacy «Эпизоды» database is renamed to «Glossn») so existing episode pages
// survive the redesign.
func (w *NotionWriter) EnsureDatabases(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.ids.Episodes != "" && w.ids.ParentPage == w.ParentPage && w.ids.Schema >= currentNotionSchema {
		return nil
	}

	if data, err := w.Meta.LoadNotionMeta(notionBootstrapKey); err == nil && len(data) > 0 {
		var stored notionDBIDs
		if jerr := json.Unmarshal(data, &stored); jerr == nil &&
			stored.ParentPage == w.ParentPage && w.databaseExists(ctx, stored.Episodes) {
			if stored.Schema < currentNotionSchema {
				if merr := w.migrateToGlossn(ctx, &stored); merr != nil {
					return merr
				}
			}
			w.ids = stored
			return nil
		}
		log.Printf("[INFO] stored notion databases stale or parent changed, re-bootstrapping")
	}

	log.Printf("[INFO] creating notion database %q under page %s", glossnDBTitle, w.ParentPage)
	// old page mappings point into deleted/stale databases — drop them so
	// repeated /notes re-creates pages instead of returning dead links
	if n, derr := w.Meta.DeleteNotionMetaPrefix("page:"); derr != nil {
		log.Printf("[WARN] failed to drop stale notion page mappings: %v", derr)
	} else if n > 0 {
		log.Printf("[INFO] dropped %d stale notion page mappings", n)
	}
	ids := notionDBIDs{ParentPage: w.ParentPage, Schema: currentNotionSchema, CreatedAt: time.Now()}

	episodesID, err := w.createDatabase(ctx, glossnDBTitle, map[string]any{
		"Name":               map[string]any{"title": map[string]any{}},
		"URL":                map[string]any{"url": map[string]any{}},
		"Канал":              map[string]any{"rich_text": map[string]any{}},
		"Дата":               map[string]any{"date": map[string]any{}},
		"Длительность (мин)": map[string]any{"number": map[string]any{}},
		"Теги":               map[string]any{"multi_select": map[string]any{}},
	})
	if err != nil {
		return fmt.Errorf("failed to create %s db: %w", glossnDBTitle, err)
	}
	ids.Episodes = episodesID

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

// migrateToGlossn upgrades a pre-schema-2 bootstrap: the legacy «Эпизоды»
// database is renamed to «Glossn» in place (episode pages are untouched), the
// orphaned References/Objects databases are simply left behind, and the schema
// version is bumped and persisted. Must be called with w.mu held.
func (w *NotionWriter) migrateToGlossn(ctx context.Context, ids *notionDBIDs) error {
	log.Printf("[INFO] migrating notion schema %d→%d: renaming episodes db to %q",
		ids.Schema, currentNotionSchema, glossnDBTitle)
	if err := w.doNotion(ctx, "PATCH", "/databases/"+ids.Episodes,
		map[string]any{"title": richText(glossnDBTitle)}, nil); err != nil {
		return fmt.Errorf("failed to rename episodes db to %s: %w", glossnDBTitle, err)
	}
	ids.Schema = currentNotionSchema
	data, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("failed to marshal migrated db ids: %w", err)
	}
	if err := w.Meta.SaveNotionMeta(notionBootstrapKey, data); err != nil {
		return fmt.Errorf("failed to persist migrated db ids: %w", err)
	}
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

	if len(in.Refs) > 0 {
		if err := w.writeReferencesInPage(ctx, pageID, in.Refs); err != nil {
			return pageURL, true, fmt.Errorf("failed to write references: %w", err)
		}
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

// createEpisodePage creates the page with properties, summary and two empty
// toggles — «📎 Отсылки (N)» (only when refs exist) and «Транскрипт» — that are
// filled separately due to the 100-blocks-per-request limit.
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
	// leave room for the heading plus up to two toggles when capping the summary
	reserve := 2
	if len(in.Refs) > 0 {
		reserve = 3
	}
	summaryBlocks := mdToParagraphBlocks(in.Summary)
	if len(summaryBlocks) > notionBlockBatch-reserve {
		summaryBlocks = summaryBlocks[:notionBlockBatch-reserve]
	}
	children = append(children, summaryBlocks...)
	if len(in.Refs) > 0 {
		children = append(children, toggleBlock(refsToggleLabel(len(in.Refs))))
	}
	children = append(children, toggleBlock("Транскрипт"))

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

// refsToggleLabel is the display label of the references toggle, distinct enough
// (prefix «📎 Отсылки») for findToggleID to tell it from the transcript toggle.
func refsToggleLabel(n int) string {
	return fmt.Sprintf("📎 Отсылки (%d)", n)
}

// appendTranscript finds the «Транскрипт» toggle on the page and fills it in
// batches of at most 100 blocks
func (w *NotionWriter) appendTranscript(ctx context.Context, pageID, transcript string) error {
	toggleID, err := w.findToggleID(ctx, pageID, "Транскрипт")
	if err != nil {
		return err
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

// writeReferencesInPage fills the «📎 Отсылки» toggle with references grouped by
// type (a heading per group + one bullet per mention). No separate database rows
// or object dedup — everything lives inside the episode page.
func (w *NotionWriter) writeReferencesInPage(ctx context.Context, pageID string, refs []Reference) error {
	toggleID, err := w.findToggleID(ctx, pageID, "📎 Отсылки")
	if err != nil {
		return err
	}
	if toggleID == "" {
		return fmt.Errorf("references toggle not found on page %s", pageID)
	}
	blocks := referenceBlocks(refs)
	if len(blocks) == 0 {
		return nil
	}
	for _, batch := range batchBlocks(blocks, notionBlockBatch) {
		if err := w.doNotion(ctx, "PATCH", "/blocks/"+toggleID+"/children",
			map[string]any{"children": batch}, nil); err != nil {
			return err
		}
	}
	return nil
}

// findToggleID returns the id of the first toggle on the page whose label starts
// with prefix (the page carries at most two toggles: «📎 Отсылки» and «Транскрипт»)
func (w *NotionWriter) findToggleID(ctx context.Context, pageID, prefix string) (string, error) {
	var list struct {
		Results []struct {
			ID     string `json:"id"`
			Type   string `json:"type"`
			Toggle struct {
				RichText []struct {
					PlainText string `json:"plain_text"`
				} `json:"rich_text"`
			} `json:"toggle"`
		} `json:"results"`
	}
	if err := w.doNotion(ctx, "GET", "/blocks/"+pageID+"/children?page_size=100", nil, &list); err != nil {
		return "", fmt.Errorf("failed to list page children: %w", err)
	}
	for _, b := range list.Results {
		if b.Type != "toggle" {
			continue
		}
		var label strings.Builder
		for _, rt := range b.Toggle.RichText {
			label.WriteString(rt.PlainText)
		}
		if strings.HasPrefix(label.String(), prefix) {
			return b.ID, nil
		}
	}
	return "", nil
}

// referenceBlocks renders references as grouped bullets: for each non-empty type
// (in referenceTypes order) a heading «{emoji} {Тип}» followed by one bullet per
// mention, formatted «Name» [timecode] — «quote».
func referenceBlocks(refs []Reference) []map[string]any {
	byType := map[string][]Reference{}
	for _, ref := range refs {
		if strings.TrimSpace(ref.Name) == "" {
			continue
		}
		t := normalizeRefType(ref.Type)
		byType[t] = append(byType[t], ref)
	}
	var blocks []map[string]any
	for _, t := range referenceTypes {
		group := byType[t]
		if len(group) == 0 {
			continue
		}
		emoji := refTypeEmoji[t]
		blocks = append(blocks, heading3Block(strings.TrimSpace(emoji+" "+capitalizeFirst(t))))
		for _, ref := range group {
			blocks = append(blocks, bulletBlock(formatReference(ref)))
		}
	}
	return blocks
}

// capitalizeFirst upper-cases the first rune (byte-slicing would split cyrillic)
func capitalizeFirst(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(unicode.ToUpper(r[0])) + string(r[1:])
}

// formatReference renders one mention: «Name» [timecode] — «quote» (parts absent
// when empty)
func formatReference(ref Reference) string {
	var b strings.Builder
	b.WriteString("«" + strings.TrimSpace(ref.Name) + "»")
	if tc := strings.TrimSpace(ref.Timecode); tc != "" {
		b.WriteString(" [" + tc + "]")
	}
	if q := strings.TrimSpace(ref.Quote); q != "" {
		b.WriteString(" — «" + q + "»")
	}
	return b.String()
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

func heading3Block(text string) map[string]any {
	return map[string]any{
		"object":    "block",
		"type":      "heading_3",
		"heading_3": map[string]any{"rich_text": richText(text)},
	}
}

func bulletBlock(text string) map[string]any {
	return map[string]any{
		"object":             "block",
		"type":               "bulleted_list_item",
		"bulleted_list_item": map[string]any{"rich_text": richText(text)},
	}
}

// toggleBlock builds an empty toggle with the given label
func toggleBlock(label string) map[string]any {
	return map[string]any{
		"object": "block",
		"type":   "toggle",
		"toggle": map[string]any{"rich_text": richText(label)},
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
