// Package store is the SQLite persistence layer (pure Go driver, no cgo).
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS providers (
	id TEXT PRIMARY KEY, name TEXT, kind TEXT, base_url TEXT, model TEXT,
	vision INTEGER, temperature REAL, effort TEXT);
CREATE TABLE IF NOT EXISTS knowledge_bases (
	id TEXT PRIMARY KEY, name TEXT, description TEXT, overview TEXT DEFAULT '', created_at INTEGER);
CREATE TABLE IF NOT EXISTS sources (
	id TEXT PRIMARY KEY, kb_id TEXT, filename TEXT, path TEXT, comic_mode TEXT,
	status TEXT, progress TEXT DEFAULT '', error TEXT DEFAULT '', pages INTEGER DEFAULT 0, created_at INTEGER);
CREATE TABLE IF NOT EXISTS characters (
	id TEXT PRIMARY KEY, name TEXT, avatar TEXT, tagline TEXT, description TEXT, personality TEXT,
	speaking_style TEXT, greeting TEXT, example_dialogue TEXT, kb_ids TEXT, created_at INTEGER);
CREATE TABLE IF NOT EXISTS scenarios (
	id TEXT PRIMARY KEY, title TEXT, setting TEXT, opening TEXT, kb_ids TEXT, created_at INTEGER);
CREATE TABLE IF NOT EXISTS chats (
	id TEXT PRIMARY KEY, character_id TEXT, scenario_id TEXT, persona_name TEXT, persona_desc TEXT,
	title TEXT, updated_at INTEGER);
CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT, chat_id TEXT, role TEXT, content TEXT,
	summarized INTEGER DEFAULT 0, created_at INTEGER);
CREATE INDEX IF NOT EXISTS idx_messages_chat ON messages(chat_id, id);
CREATE INDEX IF NOT EXISTS idx_sources_kb ON sources(kb_id);
`

// Columns added after the first release. SQLite has no ADD COLUMN IF NOT EXISTS,
// so each is tried and "duplicate column" errors are ignored.
var migrations = []string{
	`ALTER TABLE knowledge_bases ADD COLUMN llm_provider TEXT DEFAULT ''`,
	`ALTER TABLE knowledge_bases ADD COLUMN vision_provider TEXT DEFAULT ''`,
	`ALTER TABLE knowledge_bases ADD COLUMN embed_provider TEXT DEFAULT ''`,
	`ALTER TABLE knowledge_bases ADD COLUMN embed_model TEXT DEFAULT ''`,
	`ALTER TABLE chats ADD COLUMN provider_id TEXT DEFAULT ''`,
	`ALTER TABLE chats ADD COLUMN mode TEXT DEFAULT ''`,
	`ALTER TABLE scenarios ADD COLUMN cast_json TEXT DEFAULT '[]'`,
	`ALTER TABLE providers ADD COLUMN thinking TEXT DEFAULT ''`,
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: serialize writers
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func NewID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func now() int64 { return time.Now().Unix() }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func encodeIDs(ids []string) string {
	if ids == nil {
		ids = []string{}
	}
	b, _ := json.Marshal(ids)
	return string(b)
}

func decodeIDs(s string) []string {
	var ids []string
	json.Unmarshal([]byte(s), &ids)
	if ids == nil {
		ids = []string{}
	}
	return ids
}

// ---- settings ----

func (s *Store) Setting(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (s *Store) Settings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---- providers ----

func (s *Store) Providers() ([]Provider, error) {
	rows, err := s.db.Query(`SELECT id,name,kind,base_url,model,vision,temperature,effort,thinking FROM providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		var p Provider
		var vision int
		if err := rows.Scan(&p.ID, &p.Name, &p.Kind, &p.BaseURL, &p.Model, &vision, &p.Temperature, &p.Effort, &p.Thinking); err != nil {
			return nil, err
		}
		p.Vision = vision == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Provider(id string) (Provider, error) {
	var p Provider
	var vision int
	err := s.db.QueryRow(`SELECT id,name,kind,base_url,model,vision,temperature,effort,thinking FROM providers WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.Kind, &p.BaseURL, &p.Model, &vision, &p.Temperature, &p.Effort, &p.Thinking)
	p.Vision = vision == 1
	return p, err
}

func (s *Store) SaveProvider(p *Provider) error {
	if p.ID == "" {
		p.ID = NewID()
	}
	_, err := s.db.Exec(`INSERT INTO providers(id,name,kind,base_url,model,vision,temperature,effort,thinking) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, kind=excluded.kind, base_url=excluded.base_url,
		model=excluded.model, vision=excluded.vision, temperature=excluded.temperature, effort=excluded.effort, thinking=excluded.thinking`,
		p.ID, p.Name, p.Kind, p.BaseURL, p.Model, b2i(p.Vision), p.Temperature, p.Effort, p.Thinking)
	return err
}

func (s *Store) DeleteProvider(id string) error {
	_, err := s.db.Exec(`DELETE FROM providers WHERE id=?`, id)
	return err
}

// ---- knowledge bases & sources ----

const kbCols = `id,name,description,overview,created_at,llm_provider,vision_provider,embed_provider,embed_model`

func scanKB(sc interface{ Scan(...any) error }) (KnowledgeBase, error) {
	var k KnowledgeBase
	err := sc.Scan(&k.ID, &k.Name, &k.Description, &k.Overview, &k.CreatedAt,
		&k.LLMProvider, &k.VisionProvider, &k.EmbedProvider, &k.EmbedModel)
	return k, err
}

func (s *Store) KnowledgeBases() ([]KnowledgeBase, error) {
	rows, err := s.db.Query(`SELECT ` + kbCols + ` FROM knowledge_bases ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []KnowledgeBase{}
	for rows.Next() {
		k, err := scanKB(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) KnowledgeBase(id string) (KnowledgeBase, error) {
	return scanKB(s.db.QueryRow(`SELECT `+kbCols+` FROM knowledge_bases WHERE id=?`, id))
}

func (s *Store) SaveKnowledgeBase(k *KnowledgeBase) error {
	if k.ID == "" {
		k.ID = NewID()
		k.CreatedAt = now()
	}
	_, err := s.db.Exec(`INSERT INTO knowledge_bases(`+kbCols+`) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, description=excluded.description, overview=excluded.overview,
		llm_provider=excluded.llm_provider, vision_provider=excluded.vision_provider,
		embed_provider=excluded.embed_provider, embed_model=excluded.embed_model`,
		k.ID, k.Name, k.Description, k.Overview, k.CreatedAt, k.LLMProvider, k.VisionProvider, k.EmbedProvider, k.EmbedModel)
	return err
}

func (s *Store) SetKBOverview(id, overview string) error {
	_, err := s.db.Exec(`UPDATE knowledge_bases SET overview=? WHERE id=?`, overview, id)
	return err
}

func (s *Store) DeleteKnowledgeBase(id string) error {
	if _, err := s.db.Exec(`DELETE FROM sources WHERE kb_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM knowledge_bases WHERE id=?`, id)
	return err
}

const sourceCols = `id,kb_id,filename,path,comic_mode,status,progress,error,pages,created_at`

func scanSource(sc interface{ Scan(...any) error }) (Source, error) {
	var x Source
	err := sc.Scan(&x.ID, &x.KBID, &x.Filename, &x.Path, &x.ComicMode, &x.Status, &x.Progress, &x.Error, &x.Pages, &x.CreatedAt)
	return x, err
}

func (s *Store) Sources(kbID string) ([]Source, error) {
	rows, err := s.db.Query(`SELECT `+sourceCols+` FROM sources WHERE kb_id=? ORDER BY created_at`, kbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Source{}
	for rows.Next() {
		x, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) Source(id string) (Source, error) {
	return scanSource(s.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE id=?`, id))
}

func (s *Store) InsertSource(x *Source) error {
	if x.ID == "" {
		x.ID = NewID()
	}
	x.CreatedAt = now()
	_, err := s.db.Exec(`INSERT INTO sources(`+sourceCols+`) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		x.ID, x.KBID, x.Filename, x.Path, x.ComicMode, x.Status, x.Progress, x.Error, x.Pages, x.CreatedAt)
	return err
}

func (s *Store) UpdateSourceStatus(id, status, progress, errMsg string) error {
	_, err := s.db.Exec(`UPDATE sources SET status=?, progress=?, error=? WHERE id=?`, status, progress, errMsg, id)
	return err
}

func (s *Store) SetSourcePages(id string, pages int) error {
	_, err := s.db.Exec(`UPDATE sources SET pages=? WHERE id=?`, pages, id)
	return err
}

func (s *Store) DeleteSource(id string) error {
	_, err := s.db.Exec(`DELETE FROM sources WHERE id=?`, id)
	return err
}

// MarkInterruptedSources flags sources that were mid-ingest when the app closed.
func (s *Store) MarkInterruptedSources() error {
	_, err := s.db.Exec(`UPDATE sources SET status='error', error='Bị gián đoạn khi đóng ứng dụng — hãy xử lý lại'
		WHERE status NOT IN ('done','error')`)
	return err
}

// ---- characters ----

const charCols = `id,name,avatar,tagline,description,personality,speaking_style,greeting,example_dialogue,kb_ids,created_at`

func scanCharacter(sc interface{ Scan(...any) error }) (Character, error) {
	var c Character
	var kb string
	err := sc.Scan(&c.ID, &c.Name, &c.Avatar, &c.Tagline, &c.Description, &c.Personality, &c.SpeakingStyle,
		&c.Greeting, &c.ExampleDialogue, &kb, &c.CreatedAt)
	c.KBIDs = decodeIDs(kb)
	return c, err
}

func (s *Store) Characters() ([]Character, error) {
	rows, err := s.db.Query(`SELECT ` + charCols + ` FROM characters ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Character{}
	for rows.Next() {
		c, err := scanCharacter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Character(id string) (Character, error) {
	return scanCharacter(s.db.QueryRow(`SELECT `+charCols+` FROM characters WHERE id=?`, id))
}

func (s *Store) SaveCharacter(c *Character) error {
	if c.ID == "" {
		c.ID = NewID()
		c.CreatedAt = now()
	}
	_, err := s.db.Exec(`INSERT INTO characters(`+charCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, avatar=excluded.avatar, tagline=excluded.tagline,
		description=excluded.description, personality=excluded.personality, speaking_style=excluded.speaking_style,
		greeting=excluded.greeting, example_dialogue=excluded.example_dialogue, kb_ids=excluded.kb_ids`,
		c.ID, c.Name, c.Avatar, c.Tagline, c.Description, c.Personality, c.SpeakingStyle, c.Greeting,
		c.ExampleDialogue, encodeIDs(c.KBIDs), c.CreatedAt)
	return err
}

func (s *Store) DeleteCharacter(id string) error {
	_, err := s.db.Exec(`DELETE FROM characters WHERE id=?`, id)
	return err
}

// ---- scenarios ----

const scenarioCols = `id,title,setting,opening,kb_ids,cast_json,created_at`

func scanScenario(sc interface{ Scan(...any) error }) (Scenario, error) {
	var x Scenario
	var kb, cast string
	err := sc.Scan(&x.ID, &x.Title, &x.Setting, &x.Opening, &kb, &cast, &x.CreatedAt)
	x.KBIDs = decodeIDs(kb)
	json.Unmarshal([]byte(cast), &x.Cast)
	if x.Cast == nil {
		x.Cast = []CastMember{}
	}
	return x, err
}

func (s *Store) Scenarios() ([]Scenario, error) {
	rows, err := s.db.Query(`SELECT ` + scenarioCols + ` FROM scenarios ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Scenario{}
	for rows.Next() {
		x, err := scanScenario(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) Scenario(id string) (Scenario, error) {
	return scanScenario(s.db.QueryRow(`SELECT `+scenarioCols+` FROM scenarios WHERE id=?`, id))
}

func (s *Store) SaveScenario(x *Scenario) error {
	if x.ID == "" {
		x.ID = NewID()
		x.CreatedAt = now()
	}
	if x.Cast == nil {
		x.Cast = []CastMember{}
	}
	cast, _ := json.Marshal(x.Cast)
	_, err := s.db.Exec(`INSERT INTO scenarios(`+scenarioCols+`) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET title=excluded.title, setting=excluded.setting, opening=excluded.opening,
		kb_ids=excluded.kb_ids, cast_json=excluded.cast_json`,
		x.ID, x.Title, x.Setting, x.Opening, encodeIDs(x.KBIDs), string(cast), x.CreatedAt)
	return err
}

func (s *Store) DeleteScenario(id string) error {
	_, err := s.db.Exec(`DELETE FROM scenarios WHERE id=?`, id)
	return err
}

// ---- chats & messages ----

const chatCols = `id,mode,character_id,scenario_id,persona_name,persona_desc,title,provider_id,updated_at`

func scanChat(sc interface{ Scan(...any) error }) (Chat, error) {
	var c Chat
	err := sc.Scan(&c.ID, &c.Mode, &c.CharacterID, &c.ScenarioID, &c.PersonaName, &c.PersonaDesc, &c.Title, &c.ProviderID, &c.UpdatedAt)
	return c, err
}

func (s *Store) Chats() ([]Chat, error) {
	rows, err := s.db.Query(`SELECT ` + chatCols + ` FROM chats ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Chat(id string) (Chat, error) {
	return scanChat(s.db.QueryRow(`SELECT `+chatCols+` FROM chats WHERE id=?`, id))
}

func (s *Store) SaveChat(c *Chat) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	c.UpdatedAt = now()
	_, err := s.db.Exec(`INSERT INTO chats(`+chatCols+`) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET persona_name=excluded.persona_name, persona_desc=excluded.persona_desc,
		title=excluded.title, provider_id=excluded.provider_id, updated_at=excluded.updated_at`,
		c.ID, c.Mode, c.CharacterID, c.ScenarioID, c.PersonaName, c.PersonaDesc, c.Title, c.ProviderID, c.UpdatedAt)
	return err
}

// SetChatProvider switches the model a conversation uses.
func (s *Store) SetChatProvider(id, providerID string) error {
	_, err := s.db.Exec(`UPDATE chats SET provider_id=? WHERE id=?`, providerID, id)
	return err
}

func (s *Store) TouchChat(id string) {
	s.db.Exec(`UPDATE chats SET updated_at=? WHERE id=?`, now(), id)
}

func (s *Store) DeleteChat(id string) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE chat_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM chats WHERE id=?`, id)
	return err
}

func (s *Store) AddMessage(chatID, role, content string) (Message, error) {
	m := Message{ChatID: chatID, Role: role, Content: content, CreatedAt: now()}
	res, err := s.db.Exec(`INSERT INTO messages(chat_id,role,content,created_at) VALUES(?,?,?,?)`, chatID, role, content, m.CreatedAt)
	if err != nil {
		return m, err
	}
	m.ID, _ = res.LastInsertId()
	s.TouchChat(chatID)
	return m, nil
}

func (s *Store) UpdateMessage(id int64, content string) error {
	_, err := s.db.Exec(`UPDATE messages SET content=? WHERE id=?`, content, id)
	return err
}

func (s *Store) DeleteMessage(id int64) error {
	_, err := s.db.Exec(`DELETE FROM messages WHERE id=?`, id)
	return err
}

// Messages returns the chat history; when onlyActive is true, messages already
// folded into long-term memory are skipped.
func (s *Store) Messages(chatID string, onlyActive bool) ([]Message, error) {
	q := `SELECT id,chat_id,role,content,summarized,created_at FROM messages WHERE chat_id=?`
	if onlyActive {
		q += ` AND summarized=0`
	}
	rows, err := s.db.Query(q+` ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var sum int
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Content, &sum, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Summarized = sum == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) MarkSummarized(chatID string, upToID int64) error {
	_, err := s.db.Exec(`UPDATE messages SET summarized=1 WHERE chat_id=? AND id<=?`, chatID, upToID)
	return err
}
