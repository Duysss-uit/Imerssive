// Package app is the thin facade bound to the UI by Wails. Every exported
// method on App is callable from JavaScript as window.go.app.App.<Method>.
// Business logic lives in the other internal packages.
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"immersive/internal/applog"
	"immersive/internal/chat"
	"immersive/internal/config"
	"immersive/internal/ingest"
	"immersive/internal/llm"
	"immersive/internal/rag"
	"immersive/internal/secrets"
	"immersive/internal/store"
	"immersive/internal/worker"
)

// Defaults for every setting the UI can change.
var defaultSettings = map[string]string{
	"provider.chat":        "",
	"provider.summarize":   "", // empty = same as chat
	"provider.vision":      "", // empty = same as chat
	"embed.text_model":     "sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2",
	"embed.image_model":    "Qdrant/clip-ViT-B-32-vision",
	"ingest.section_chars": "16000",
	"ingest.chunk_chars":   "1500",
	"ingest.keep_raw":      "true",
	"ingest.concurrency":   "3",
	"ingest.vision_batch":  "4",
	"chat.rag_k":           "8",
	"chat.context_chars":   "10000",
	"chat.window":          "40",
	"chat.max_tokens":      "6000",
	"chat.attach_images":   "false",
	"chat.image_k":         "2",
	"worker.idle_minutes":  "5",
	"worker.python":        "",               // empty = auto-detect worker/.venv
	"log.model":            applog.ModelFull, // full | response | off
}

type App struct {
	ctx    context.Context
	logs   *applog.Logs
	paths  config.Paths
	st     *store.Store
	w      *worker.Manager
	rag    *rag.Store
	ingest *ingest.Service
	chat   *chat.Engine
}

func New() (*App, error) {
	paths, err := config.Resolve()
	if err != nil {
		return nil, err
	}
	logs, err := applog.Setup(paths.Logs)
	if err != nil {
		return nil, err
	}
	log.Printf("app: khởi động, dữ liệu tại %s", paths.DataDir)
	st, err := store.Open(paths.DB)
	if err != nil {
		return nil, err
	}
	st.MarkInterruptedSources()

	a := &App{paths: paths, st: st, logs: logs}
	a.w = worker.New(a.workerOptions())
	a.rag = rag.New(a.w)
	a.ingest = ingest.New(st, a.w, a.rag, a.space, a.resolve, a.ingestOptions, paths.Sources, paths.Pages, a.emit)
	a.chat = chat.New(st, a.rag, a.space, a.resolve, a.chatOptions, a.emit)
	return a, nil
}

func (a *App) Startup(ctx context.Context) { a.ctx = ctx }

func (a *App) Shutdown(ctx context.Context) {
	log.Printf("app: tắt")
	a.w.Stop()
	a.st.Close()
	a.logs.Close()
}

func (a *App) emit(event string, data any) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, event, data)
	}
}

// ---- settings ----

func (a *App) setting(key string) string {
	if v := a.st.Setting(key); v != "" {
		return v
	}
	return defaultSettings[key]
}

func (a *App) intSetting(key string) int {
	n, err := strconv.Atoi(a.setting(key))
	if err != nil {
		n, _ = strconv.Atoi(defaultSettings[key])
	}
	return n
}

func (a *App) workerOptions() worker.Options {
	dir, python := config.FindWorker()
	if p := a.setting("worker.python"); p != "" {
		python = p
	}
	return worker.Options{
		Python: python, Dir: dir,
		IdleFor: time.Duration(a.intSetting("worker.idle_minutes")) * time.Minute,
		Env: []string{
			"IMM_CHROMA_DIR=" + a.paths.Chroma,
			"IMM_MODELS_DIR=" + a.paths.ModelsDir,
			"IMM_TEXT_MODEL=" + a.setting("embed.text_model"),
			"IMM_IMAGE_MODEL=" + a.setting("embed.image_model"),
		},
	}
}

func (a *App) ingestOptions() ingest.Options {
	return ingest.Options{
		SectionChars: a.intSetting("ingest.section_chars"),
		ChunkChars:   a.intSetting("ingest.chunk_chars"),
		KeepRaw:      a.setting("ingest.keep_raw") == "true",
		Concurrency:  a.intSetting("ingest.concurrency"),
		VisionBatch:  a.intSetting("ingest.vision_batch"),
	}
}

func (a *App) chatOptions() chat.Options {
	return chat.Options{
		RAGK:         a.intSetting("chat.rag_k"),
		ContextChars: a.intSetting("chat.context_chars"),
		Window:       max(a.intSetting("chat.window"), 6),
		MaxTokens:    a.intSetting("chat.max_tokens"),
		AttachImages: a.setting("chat.attach_images") == "true",
		ImageK:       a.intSetting("chat.image_k"),
	}
}

func (a *App) GetSettings() map[string]string {
	out := map[string]string{}
	for k, v := range defaultSettings {
		out[k] = v
	}
	stored, _ := a.st.Settings()
	for k, v := range stored {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

func (a *App) SaveSettings(values map[string]string) error {
	workerChanged := false
	for k, v := range values {
		if _, ok := defaultSettings[k]; !ok {
			continue
		}
		if a.setting(k) != v && (k == "embed.text_model" || k == "embed.image_model" || k == "worker.python" || k == "worker.idle_minutes") {
			workerChanged = true
		}
		if err := a.st.SetSetting(k, v); err != nil {
			return err
		}
	}
	log.Printf("settings: đã lưu %d mục", len(values))
	if workerChanged {
		a.w.Reconfigure(a.workerOptions())
	}
	return nil
}

type WorkerStatus struct {
	Running bool   `json:"running"`
	Dir     string `json:"dir"`
	Python  string `json:"python"`
}

func (a *App) GetWorkerStatus() WorkerStatus {
	o := a.workerOptions()
	return WorkerStatus{Running: a.w.Running(), Dir: o.Dir, Python: o.Python}
}

// ---- providers ----

type ProviderInput struct {
	store.Provider
	APIKey string `json:"apiKey"` // empty on update = keep the stored key
}

func (a *App) ListProviders() ([]store.Provider, error) {
	ps, err := a.st.Providers()
	for i := range ps {
		k, _ := secrets.Get(ps[i].ID)
		ps[i].HasKey = k != ""
	}
	return ps, err
}

func (a *App) SaveProvider(in ProviderInput) (store.Provider, error) {
	p := in.Provider
	if p.BaseURL == "" {
		p.BaseURL = llm.DefaultBaseURL(llm.Kind(p.Kind))
	}
	if p.Kind == string(llm.KindAnthropic) {
		p.Vision = true
	}
	if err := a.st.SaveProvider(&p); err != nil {
		return p, err
	}
	if in.APIKey != "" {
		if err := secrets.Set(p.ID, in.APIKey); err != nil {
			return p, fmt.Errorf("lưu API key vào Credential Manager: %w", err)
		}
	}
	log.Printf("provider: lưu %q (%s · %s)", p.Name, p.Kind, p.Model)
	// First provider becomes the default chat provider.
	if a.st.Setting("provider.chat") == "" {
		a.st.SetSetting("provider.chat", p.ID)
	}
	return p, nil
}

func (a *App) DeleteProvider(id string) error {
	log.Printf("provider: xoá %s", id)
	secrets.Delete(id)
	return a.st.DeleteProvider(id)
}

func (a *App) configFor(p store.Provider, key string) llm.Config {
	return llm.Config{Kind: llm.Kind(p.Kind), BaseURL: p.BaseURL, Model: p.Model, APIKey: key,
		Vision: p.Vision, Temperature: p.Temperature, Effort: p.Effort, NoThinking: p.Thinking == "off"}
}

// resolve returns provider id, or the one assigned to role in settings when id
// is empty (summarize/vision fall back to the chat provider).
func (a *App) resolve(role, id string) (llm.Provider, error) {
	if id == "" {
		id = a.st.Setting("provider." + role)
	}
	if id == "" {
		id = a.st.Setting("provider.chat")
	}
	if id == "" {
		return nil, errors.New("chưa cấu hình provider — vào Cài đặt để thêm")
	}
	p, err := a.st.Provider(id)
	if err != nil {
		return nil, fmt.Errorf("provider cho '%s' không tồn tại", role)
	}
	return a.provider(p, role)
}

// provider builds a client for p whose calls are written to the model log.
func (a *App) provider(p store.Provider, role string) (llm.Provider, error) {
	key, err := secrets.Get(p.ID)
	if err != nil {
		return nil, err
	}
	prov, err := llm.New(a.configFor(p, key))
	if err != nil {
		return nil, err
	}
	return llm.Logged(prov, p.Name, p.Model, role, func(c llm.Call) { a.logs.Model(c, a.setting("log.model")) }), nil
}

// space returns the embedding space of a knowledge base: the local model
// unless the KB picked an embedding provider. Non-KB ids (chat memory) are local.
func (a *App) space(kbID string) (rag.Space, error) {
	kb, err := a.st.KnowledgeBase(kbID)
	if err != nil || kb.EmbedProvider == "" {
		return rag.Space{}, nil
	}
	return a.apiSpace(kb.EmbedProvider, kb.EmbedModel)
}

func (a *App) apiSpace(providerID, model string) (rag.Space, error) {
	p, err := a.st.Provider(providerID)
	if err != nil {
		return rag.Space{}, errors.New("provider embedding của bộ truyện không còn tồn tại — chọn lại trong Thư viện")
	}
	key, _ := secrets.Get(providerID)
	cfg := a.configFor(p, key)
	cfg.Model = model
	emb, err := llm.NewEmbedder(cfg)
	if err != nil {
		return rag.Space{}, err
	}
	return rag.APISpace(providerID, model, emb.Embed), nil
}

// ListProviderModels lists the models of a saved provider.
func (a *App) ListProviderModels(id string) ([]string, error) {
	p, err := a.st.Provider(id)
	if err != nil {
		return nil, err
	}
	return a.ListModels(ProviderInput{Provider: p})
}

// ListModels queries the endpoint for available models (works before saving).
func (a *App) ListModels(in ProviderInput) ([]string, error) {
	key := in.APIKey
	if key == "" && in.ID != "" {
		key, _ = secrets.Get(in.ID)
	}
	cfg := a.configFor(in.Provider, key)
	if cfg.BaseURL == "" {
		cfg.BaseURL = llm.DefaultBaseURL(cfg.Kind)
	}
	cfg.Model = "list" // llm.New requires a model
	p, err := llm.New(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return p.ListModels(ctx)
}

func (a *App) TestProvider(id string) (string, error) {
	p, err := a.st.Provider(id)
	if err != nil {
		return "", err
	}
	prov, err := a.provider(p, "test")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return llm.Complete(ctx, prov, llm.Request{Purpose: "thử provider", MaxTokens: 2000,
		Messages: []llm.Message{llm.TextMessage(llm.RoleUser, "Chào bằng một câu ngắn tiếng Việt.")}})
}

// ---- knowledge bases ----

func (a *App) ListKnowledgeBases() ([]store.KnowledgeBase, error) { return a.st.KnowledgeBases() }

// SaveKnowledgeBase saves a KB. When its embedding choice changes, the stored
// texts are re-embedded into the new space (no LLM calls) before saving.
func (a *App) SaveKnowledgeBase(kb store.KnowledgeBase) (store.KnowledgeBase, error) {
	if kb.EmbedProvider == "" {
		kb.EmbedModel = ""
	}
	old, err := a.st.KnowledgeBase(kb.ID)
	if kb.ID == "" || err != nil || (old.EmbedProvider == kb.EmbedProvider && old.EmbedModel == kb.EmbedModel) {
		return kb, a.st.SaveKnowledgeBase(&kb)
	}

	to := rag.Space{}
	if kb.EmbedProvider != "" {
		if to, err = a.apiSpace(kb.EmbedProvider, kb.EmbedModel); err != nil {
			return kb, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, err := to.Embed(ctx, []string{"kiểm tra"}, true)
		cancel()
		if err != nil {
			return kb, fmt.Errorf("thử model embedding thất bại: %w", err)
		}
	}
	srcs, err := a.st.Sources(kb.ID)
	if err != nil {
		return kb, err
	}
	for _, s := range srcs {
		if s.Status != "done" && s.Status != "error" {
			return kb, errors.New("đang xử lý tài liệu — đợi xong rồi mới đổi model embedding")
		}
	}
	if len(srcs) > 0 {
		from, err := a.space(kb.ID)
		if err != nil {
			return kb, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		log.Printf("library: embedding lại %q sang %s %s", kb.Name, or(kb.EmbedProvider, "local"), kb.EmbedModel)
		if _, err := a.rag.Move(ctx, kb.ID, from, to); err != nil {
			return kb, fmt.Errorf("embedding lại tài liệu: %w", err)
		}
	}
	return kb, a.st.SaveKnowledgeBase(&kb)
}

func (a *App) DeleteKnowledgeBase(id string) error {
	log.Printf("library: xoá bộ truyện %s", id)
	srcs, err := a.st.Sources(id)
	if err != nil {
		return err
	}
	for _, s := range srcs {
		if err := a.ingest.DeleteSource(context.Background(), s.ID); err != nil {
			return err
		}
	}
	return a.st.DeleteKnowledgeBase(id)
}

func (a *App) ListSources(kbID string) ([]store.Source, error) { return a.st.Sources(kbID) }

func (a *App) PickFiles() ([]string, error) {
	return runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "Chọn truyện / tài liệu",
		Filters: []runtime.FileFilter{
			{DisplayName: "Tài liệu & truyện tranh", Pattern: "*.pdf;*.epub;*.cbz;*.txt;*.md;*.png;*.jpg;*.jpeg;*.webp"},
		},
	})
}

// AddSources queues files for ingestion. comicMode: vision | embed | both.
func (a *App) AddSources(kbID string, paths []string, comicMode string) ([]store.Source, error) {
	var out []store.Source
	for _, p := range paths {
		s, err := a.ingest.AddSource(kbID, p, comicMode)
		if err != nil {
			return out, fmt.Errorf("%s: %w", p, err)
		}
		log.Printf("library: thêm tài liệu %s (truyện tranh: %s)", p, comicMode)
		out = append(out, s)
	}
	return out, nil
}

func (a *App) ReprocessSource(id string) error {
	log.Printf("library: xử lý lại tài liệu %s", id)
	return a.ingest.Reprocess(context.Background(), id)
}

func (a *App) DeleteSource(id string) error {
	log.Printf("library: xoá tài liệu %s", id)
	return a.ingest.DeleteSource(context.Background(), id)
}

// SearchKnowledge lets the user preview what retrieval finds.
func (a *App) SearchKnowledge(kbID, query string) ([]rag.Hit, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	hits, err := a.rag.SearchKBs(ctx, a.space, query, rag.Filter{KBIDs: []string{kbID}}, 10)
	return hits, act(err, "rag: thử truy xuất %q → %d kết quả", query, len(hits))
}

// ---- characters & scenarios ----

func (a *App) ListCharacters() ([]store.Character, error) { return a.st.Characters() }

func (a *App) SaveCharacter(c store.Character) (store.Character, error) {
	err := a.st.SaveCharacter(&c)
	return c, act(err, "character: lưu %q", c.Name)
}

func (a *App) DeleteCharacter(id string) error {
	return act(a.st.DeleteCharacter(id), "character: xoá %s", id)
}

// GenerateCharacter drafts a card with providerID (empty = summarize role).
func (a *App) GenerateCharacter(kbIDs []string, name, hint, providerID string) (store.Character, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := a.ingest.GenerateCharacter(ctx, kbIDs, name, hint, providerID)
	return c, act(err, "character: AI tạo thẻ %q", name)
}

func (a *App) ListScenarios() ([]store.Scenario, error) { return a.st.Scenarios() }

func (a *App) SaveScenario(s store.Scenario) (store.Scenario, error) {
	err := a.st.SaveScenario(&s)
	return s, act(err, "scenario: lưu %q (%d nhân vật)", s.Title, len(s.Cast))
}

func (a *App) DeleteScenario(id string) error {
	return act(a.st.DeleteScenario(id), "scenario: xoá %s", id)
}

func (a *App) GenerateScenario(kbIDs []string, hint, providerID string) (store.Scenario, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s, err := a.ingest.GenerateScenario(ctx, kbIDs, hint, providerID)
	return s, act(err, "scenario: AI tạo bối cảnh %q (%d nhân vật) từ ý tưởng %q", s.Title, len(s.Cast), hint)
}

// ExtractCast lists source characters who fit a user-written scenario.
func (a *App) ExtractCast(kbIDs []string, title, setting, providerID string) ([]store.CastMember, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cast, err := a.ingest.ExtractCast(ctx, kbIDs, title, setting, providerID)
	return cast, act(err, "scenario: AI trích xuất %d nhân vật cho %q", len(cast), title)
}

// ---- chats ----

func (a *App) ListChats() ([]store.Chat, error) { return a.st.Chats() }

func (a *App) CreateChat(c store.Chat) (store.Chat, error) {
	err := a.chat.Create(&c)
	return c, act(err, "chat: tạo %q (chế độ %s, vai %q)", c.Title, or(c.Mode, "nhân vật"), c.PersonaName)
}

func (a *App) UpdateChat(c store.Chat) error { return a.st.SaveChat(&c) }

// SetChatProvider switches a conversation's model (empty = default chat provider).
func (a *App) SetChatProvider(chatID, providerID string) error {
	return act(a.st.SetChatProvider(chatID, providerID), "chat: %s đổi model sang %s", chatID, or(providerID, "mặc định"))
}

func (a *App) DeleteChat(id string) error {
	return act(a.chat.Delete(context.Background(), id), "chat: xoá %s", id)
}

func (a *App) GetMessages(chatID string) ([]store.Message, error) {
	return a.st.Messages(chatID, false)
}

func (a *App) SendMessage(chatID, text string) (store.Message, error) {
	m, err := a.chat.Send(chatID, text)
	return m, act(err, "chat: %s người chơi gửi %d ký tự", chatID, len([]rune(text)))
}

func (a *App) Regenerate(chatID string) error {
	return act(a.chat.Regenerate(chatID), "chat: %s tạo lại câu trả lời", chatID)
}

func (a *App) StopGeneration(chatID string) {
	log.Printf("chat: %s dừng sinh câu trả lời", chatID)
	a.chat.Stop(chatID)
}

func (a *App) EditMessage(id int64, content string) error {
	return act(a.st.UpdateMessage(id, content), "chat: sửa tin nhắn %d", id)
}

func (a *App) DeleteMessage(id int64) error {
	return act(a.st.DeleteMessage(id), "chat: xoá tin nhắn %d", id)
}

func (a *App) LogDir() string { return a.paths.Logs }

// OpenLogDir shows the log folder in Explorer.
func (a *App) OpenLogDir() error {
	return exec.Command("explorer", a.paths.Logs).Start()
}

// act writes one activity line, with the error if the action failed, and returns err.
func act(err error, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if err != nil {
		log.Output(2, msg+" — LỖI: "+err.Error())
	} else {
		log.Output(2, msg)
	}
	return err
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func init() { log.SetFlags(log.LstdFlags | log.Lshortfile) }
