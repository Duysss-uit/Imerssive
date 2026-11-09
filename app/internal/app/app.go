// Package app is the thin facade bound to the UI by Wails. Every exported
// method on App is callable from JavaScript as window.go.app.App.<Method>.
// Business logic lives in the other internal packages.
package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"immersive/internal/applog"
	"immersive/internal/chat"
	"immersive/internal/config"
	"immersive/internal/ingest"
	"immersive/internal/llm"
	"immersive/internal/lore"
	"immersive/internal/prompts"
	"immersive/internal/rag"
	"immersive/internal/secrets"
	"immersive/internal/store"
	"immersive/internal/worker"
)

// Defaults for every setting the UI can change.
var defaultSettings = map[string]string{
	"provider.chat":          "",
	"provider.summarize":     "", // empty = same as chat
	"provider.vision":        "", // empty = same as chat
	"provider.image":         "", // empty = no illustrations
	"embed.text_model":       "sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2",
	"embed.image_model":      "Qdrant/clip-ViT-B-32-vision",
	"ingest.chapter_chars":   "40000",
	"ingest.chunk_chars":     "1500",
	"ingest.keep_raw":        "true",
	"ingest.concurrency":     "3",
	"ingest.vision_max_side": "1024",
	"chat.rag_k":             "8",
	"chat.context_chars":     "10000",
	"chat.window":            "40",
	"chat.max_tokens":        "6000",
	"chat.attach_images":     "false",
	"chat.image_k":           "2",
	"chat.divergence_every":  "3",
	"chat.auto_profiles":     "true",
	"image.auto":             "false", // the model may draw by itself in replies (the Tạo ảnh button works whenever an image provider is set)
	"image.max_refs":         "4",
	"image.style":            "",
	"worker.idle_minutes":    "5",
	"worker.python":          "",               // empty = auto-detect worker/.venv
	"log.model":              applog.ModelFull, // full | response | off
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

	ctxMu  sync.Mutex
	ctxWin map[string]detected // provider id + model → detected context window
}

type detected struct {
	tokens int
	at     time.Time
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
	// Prompts live next to the app so they are easy to find and edit; the data
	// folder is the fallback when the app's folder isn't writable.
	var promptDirs []string
	if exe, err := os.Executable(); err == nil {
		promptDirs = append(promptDirs, filepath.Join(filepath.Dir(exe), "prompts"))
	}
	if d, err := prompts.Init(append(promptDirs, filepath.Join(paths.DataDir, "prompts"))...); err != nil {
		log.Printf("app: không tạo được thư mục prompt, dùng prompt mặc định: %v", err)
	} else {
		log.Printf("app: prompt tại %s", d)
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		return nil, err
	}
	st.MarkInterruptedSources()
	markImageProvider(st)

	a := &App{paths: paths, st: st, logs: logs, ctxWin: map[string]detected{}}
	a.w = worker.New(a.workerOptions())
	a.rag = rag.New(a.w)
	a.ingest = ingest.New(st, a.w, a.rag, a.space, a.resolve, a.ingestOptions, paths.Sources, paths.Pages, a.emit)
	lr := &lore.Lore{St: st, Vec: a.rag, Spaces: a.space, PagesDir: paths.Pages}
	a.chat = chat.New(st, a.rag, lr, a.space, a.resolve, a.chatOptions, a.emit)
	a.chat.SetImaging(chat.Imaging{Imager: a.imager, Dir: paths.Images,
		Available: func() bool { return a.st.Setting("provider.image") != "" },
		Auto:      func() bool { return a.setting("image.auto") == "true" }})
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
		ChapterChars:  a.intSetting("ingest.chapter_chars"),
		ChunkChars:    a.intSetting("ingest.chunk_chars"),
		KeepRaw:       a.setting("ingest.keep_raw") == "true",
		Concurrency:   a.intSetting("ingest.concurrency"),
		VisionMaxSide: a.intSetting("ingest.vision_max_side"),
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

		DivergenceEvery: a.intSetting("chat.divergence_every"),
		AutoProfiles:    a.setting("chat.auto_profiles") == "true",
		ImageMaxRefs:    a.intSetting("image.max_refs"),
		ImageStyle:      a.setting("image.style"),
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
	drawOnly := p.Purpose == store.PurposeImage
	if drawOnly {
		p.Vision, p.Effort, p.Thinking, p.ContextWindow = false, "", "", 0
		if p.ImageAPI == "" {
			p.ImageAPI = llm.ImageAPIFor(llm.Kind(p.Kind), "")
		}
		if p.Kind == string(llm.KindAnthropic) {
			return p, errors.New("Anthropic không có model tạo ảnh")
		}
	} else {
		p.Purpose, p.ImageAPI, p.Workflow, p.WorkflowNoRef = "", "", "", ""
	}
	if drawOnly && p.ImageAPI == llm.ImageAPIComfyUI {
		if p.Workflow == "" && p.WorkflowNoRef == "" {
			return p, errors.New("chưa có workflow ComfyUI — dán ít nhất một workflow (API format)")
		}
		for _, w := range []string{p.Workflow, p.WorkflowNoRef} {
			if err := llm.CheckComfyWorkflow(w); err != nil {
				return p, err
			}
		}
		if llm.ComfyRefSlots(p.Workflow) == 0 && p.Workflow != "" {
			return p, errors.New("workflow “có ảnh tham chiếu” chưa có chỗ trống {ref1} nào — đặt {ref1}, {ref2}… vào ô image của các node LoadImage, hoặc dán nó vào ô “không ảnh tham chiếu”")
		}
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
	if a.st.Setting("provider.chat") == "" && !drawOnly {
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
		Vision: p.Vision, Temperature: p.Temperature, Effort: p.Effort, NoThinking: p.Thinking == "off", ContextWindow: p.ContextWindow, ImageAPI: p.ImageAPI,
		Workflow: p.Workflow, WorkflowNoRef: p.WorkflowNoRef}
}

// contextWindow is the context length prompts for p are sized to: the one
// the user set, else what the server reports (asked at most every 10
// minutes, as a local model may be reloaded with another length), else a
// cautious default.
func (a *App) contextWindow(p store.Provider, key string) int {
	if p.ContextWindow > 0 {
		return p.ContextWindow
	}
	id := p.ID + "\x00" + p.Model
	a.ctxMu.Lock()
	d, ok := a.ctxWin[id]
	a.ctxMu.Unlock()
	if !ok || time.Since(d.at) > 10*time.Minute {
		d = detected{tokens: llm.DetectContext(context.Background(), a.configFor(p, key)), at: time.Now()}
		a.ctxMu.Lock()
		a.ctxWin[id] = d
		a.ctxMu.Unlock()
		if d.tokens > 0 {
			log.Printf("provider: %q (%s) có ngữ cảnh %d token", p.Name, p.Model, d.tokens)
		} else {
			log.Printf("provider: %q (%s) không cho biết độ dài ngữ cảnh, dùng mặc định", p.Name, p.Model)
		}
	}
	if d.tokens > 0 {
		return d.tokens
	}
	if p.Kind == string(llm.KindLocal) {
		return llm.DefaultContextLocal
	}
	return llm.DefaultContextRemote
}

// DetectContextWindow asks the server for the model's context length (0 = it doesn't say).
func (a *App) DetectContextWindow(in ProviderInput) int {
	key := in.APIKey
	if key == "" && in.ID != "" {
		key, _ = secrets.Get(in.ID)
	}
	n := llm.DetectContext(context.Background(), a.configFor(in.Provider, key))
	log.Printf("provider: nhận diện ngữ cảnh %s → %d token", in.Model, n)
	return n
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
	if p.Purpose == store.PurposeImage {
		return nil, fmt.Errorf("“%s” là provider tạo ảnh, không dùng được cho %s — chọn một model ngôn ngữ", p.Name, role)
	}
	return a.provider(p, role)
}

// provider builds a client for p whose calls are written to the model log.
func (a *App) provider(p store.Provider, role string) (llm.Provider, error) {
	key, err := secrets.Get(p.ID)
	if err != nil {
		return nil, err
	}
	cfg := a.configFor(p, key)
	cfg.ContextWindow = a.contextWindow(p, key)
	if role == llm.RoleVision {
		cfg.NoThinking = true // transcribing a page needs eyes, not reasoning
	}
	prov, err := llm.New(cfg)
	if err != nil {
		return nil, err
	}
	return llm.Logged(prov, p.Name, p.Model, role, func(c llm.Call) { a.logs.Model(c, a.setting("log.model")) }), nil
}

// markImageProvider makes the provider picked for illustrations (before
// providers had a purpose) an image provider, unless a language role uses it.
func markImageProvider(st *store.Store) {
	id := st.Setting("provider.image")
	if id == "" {
		return
	}
	for _, role := range []string{"chat", "summarize", "vision"} {
		if st.Setting("provider."+role) == id {
			return
		}
	}
	if p, err := st.Provider(id); err == nil && p.Purpose == "" {
		p.Purpose = store.PurposeImage
		st.SaveProvider(&p)
	}
}

// imager is the client of the provider set for illustrations.
func (a *App) imager() (llm.Imager, error) {
	id := a.st.Setting("provider.image")
	if id == "" {
		return nil, errors.New("chưa chọn provider ảnh — vào Cài đặt › Tạo ảnh minh hoạ")
	}
	p, err := a.st.Provider(id)
	if err != nil {
		return nil, errors.New("provider tạo ảnh không còn tồn tại")
	}
	if p.Purpose != store.PurposeImage {
		return nil, fmt.Errorf("“%s” là model ngôn ngữ, không vẽ được — chọn một provider tạo ảnh", p.Name)
	}
	return a.loggedImager(p)
}

// loggedImager builds p's drawing client; every drawing goes to the model log.
func (a *App) loggedImager(p store.Provider) (llm.Imager, error) {
	key, err := secrets.Get(p.ID)
	if err != nil {
		return nil, err
	}
	cfg := a.configFor(p, key)
	im, err := llm.NewImager(cfg)
	if err != nil {
		return nil, err
	}
	return llm.LoggedImager(im, p.Name, p.Model, llm.ImageAPIFor(cfg.Kind, cfg.ImageAPI),
		func(c llm.ImageCall) { a.logs.Image(c, a.setting("log.model")) }), nil
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
	if p.Purpose == store.PurposeImage {
		return "", errors.New("đây là provider tạo ảnh — dùng nút “Vẽ thử”")
	}
	prov, err := a.provider(p, "test")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return llm.Complete(ctx, prov, llm.Request{Purpose: "thử provider", MaxTokens: 2000,
		Messages: []llm.Message{llm.TextMessage(llm.RoleUser, "Say hello in one short sentence.")}})
}

// TestImageProvider draws a small test picture with an image provider and
// returns it as a data URI.
func (a *App) TestImageProvider(id string) (string, error) {
	p, err := a.st.Provider(id)
	if err != nil {
		return "", err
	}
	im, err := a.loggedImager(p)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := time.Now()
	img, err := im.Draw(ctx, llm.ImageRequest{Purpose: "vẽ thử provider", Aspect: "square",
		Prompt: "A cozy tea house at dusk, warm lanterns, a cat sleeping on the counter, detailed illustration"})
	if err != nil {
		return "", act(err, "provider: vẽ thử %s", p.Name)
	}
	log.Printf("provider: vẽ thử %s xong sau %s", p.Name, time.Since(start).Round(time.Second))
	return "data:" + img.MIME + ";base64," + img.B64, nil
}

// ListImageModels lists the checkpoints/models an image provider offers.
func (a *App) ListImageModels(in ProviderInput) ([]string, error) {
	key := in.APIKey
	if key == "" && in.ID != "" {
		key, _ = secrets.Get(in.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return llm.ImageModels(ctx, a.configFor(in.Provider, key))
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
	out, err := a.ingest.AddSources(kbID, paths, comicMode)
	for _, s := range out {
		log.Printf("library: thêm tài liệu %s (phần %d, truyện tranh: %s)", s.Filename, s.Seq, comicMode)
	}
	return out, err
}

// ResummarizeKB summarizes a work again from its first part, as one story.
func (a *App) ResummarizeKB(kbID string) error {
	return act(a.ingest.Resummarize(kbID), "library: tóm tắt lại bộ truyện %s từ đầu", kbID)
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s, err := a.ingest.GenerateScenario(ctx, kbIDs, hint, providerID)
	return s, act(err, "scenario: AI tạo bối cảnh %q (%d nhân vật) từ ý tưởng %q", s.Title, len(s.Cast), hint)
}

// ExtractCast lists source characters who fit a user-written scenario.
func (a *App) ExtractCast(kbIDs []string, title, setting, providerID string) ([]store.CastMember, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cast, err := a.ingest.ExtractCast(ctx, kbIDs, title, setting, providerID)
	return cast, act(err, "scenario: AI trích xuất %d nhân vật cho %q", len(cast), title)
}

// ---- chats ----

func (a *App) ListChats() ([]store.Chat, error) { return a.st.Chats() }

func (a *App) CreateChat(c store.Chat) (store.Chat, error) {
	err := a.chat.Create(&c)
	if err == nil && c.Mode == store.ModeScene {
		err = a.chat.Begin(c.ID) // no written opening: the narrator opens
	}
	return c, act(err, "chat: tạo %q (chế độ %s, vai %q)", c.Title, or(c.Mode, "nhân vật"), c.PersonaName)
}

// KBCharacters lists the characters profiled in the knowledge bases, ready to
// play or to be played against (no model call: the profiles came from ingest).
func (a *App) KBCharacters(kbIDs []string) ([]store.CastMember, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ents, err := a.ingest.Entities(ctx, kbIDs)
	out := []store.CastMember{}
	for _, e := range ents {
		if e.IsCharacter() {
			out = append(out, store.CastMember{Name: e.Name, Description: ingest.Shorten(e.Profile, 600)})
		}
	}
	return out, err
}

type QuickPlayInput struct {
	KBIDs       []string `json:"kbIds"`
	Start       string   `json:"start"` // where to begin, in the player's words; may be empty
	PersonaName string   `json:"personaName"`
	PersonaDesc string   `json:"personaDesc"`
	ProviderID  string   `json:"providerId"`
}

// QuickPlay starts a scene straight from knowledge bases: no scenario to write
// or generate. The cast is every profiled character and the narrator opens
// the story itself, retrieving what it needs.
func (a *App) QuickPlay(in QuickPlayInput) (store.Chat, error) {
	if len(in.KBIDs) == 0 {
		return store.Chat{}, errors.New("chọn ít nhất một bộ truyện")
	}
	cast, err := a.KBCharacters(in.KBIDs)
	if err != nil {
		return store.Chat{}, err
	}
	var names []string
	for _, id := range in.KBIDs {
		if kb, err := a.st.KnowledgeBase(id); err == nil {
			names = append(names, kb.Name)
		}
	}
	title := strings.Join(names, " + ")
	if s := strings.TrimSpace(in.Start); s != "" {
		title += " · " + ingest.Shorten(s, 40)
	}
	sc := store.Scenario{Title: title, Setting: strings.TrimSpace(in.Start), KBIDs: in.KBIDs, Cast: cast[:min(len(cast), 12)], Quick: true}
	if err := a.st.SaveScenario(&sc); err != nil {
		return store.Chat{}, err
	}
	return a.CreateChat(store.Chat{Mode: store.ModeScene, ScenarioID: sc.ID, ProviderID: in.ProviderID,
		PersonaName: in.PersonaName, PersonaDesc: in.PersonaDesc})
}

func (a *App) UpdateChat(c store.Chat) error { return a.st.SaveChat(&c) }

// SetChatProvider switches a conversation's model (empty = default chat provider).
func (a *App) SetChatProvider(chatID, providerID string) error {
	return act(a.st.SetChatProvider(chatID, providerID), "chat: %s đổi model sang %s", chatID, or(providerID, "mặc định"))
}

func (a *App) DeleteChat(id string) error {
	c, _ := a.st.Chat(id)
	if err := act(a.chat.Delete(context.Background(), id), "chat: xoá %s", id); err != nil {
		return err
	}
	// A "Chơi ngay" scenario exists only for its chats.
	if sc, err := a.st.Scenario(c.ScenarioID); err == nil && sc.Quick {
		chats, _ := a.st.Chats()
		if !slices.ContainsFunc(chats, func(x store.Chat) bool { return x.ScenarioID == sc.ID }) {
			a.st.DeleteScenario(sc.ID)
		}
	}
	return nil
}

func (a *App) GetMessages(chatID string) ([]store.Message, error) {
	return a.st.Messages(chatID, false)
}

func (a *App) SendMessage(chatID, text string) (store.Message, error) {
	m, err := a.chat.Send(chatID, text)
	return m, act(err, "chat: %s người chơi gửi %d ký tự", chatID, len([]rune(text)))
}

// ContinueChat has the model carry the story on without a player message.
func (a *App) ContinueChat(chatID string) error {
	return act(a.chat.Continue(chatID), "chat: %s tiếp tục (không nhập gì)", chatID)
}

// RollBackChat deletes the messages after msgID so the chat goes on from there.
func (a *App) RollBackChat(chatID string, msgID int64) error {
	return act(a.chat.RollBack(context.Background(), chatID, msgID), "chat: %s quay lại tin nhắn %d", chatID, msgID)
}

// BranchChat starts a new chat from this one as it was at msgID.
func (a *App) BranchChat(chatID string, msgID int64) (store.Chat, error) {
	c, err := a.chat.Branch(chatID, msgID)
	return c, act(err, "chat: %s rẽ nhánh tại tin nhắn %d → %s", chatID, msgID, c.ID)
}

// IllustrateMessage draws the moment of a message (the player's or a reply)
// and adds the picture to it.
func (a *App) IllustrateMessage(chatID string, msgID int64) error {
	return act(a.chat.Illustrate(chatID, msgID), "chat: %s minh hoạ tin nhắn %d", chatID, msgID)
}

// ChatImage returns an illustration of a chat as a data URI.
func (a *App) ChatImage(rel string) (string, error) {
	p, err := a.chat.ImagePath(rel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("không đọc được ảnh: %w", err)
	}
	return "data:" + http.DetectContentType(data) + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// ImagesAvailable says whether an image provider is set (the Tạo ảnh button works).
func (a *App) ImagesAvailable() bool { return a.st.Setting("provider.image") != "" }

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

// SetChatDivergence saves the player's version of a chat's divergence log.
func (a *App) SetChatDivergence(chatID, text string) error {
	return act(a.chat.SetDivergence(chatID, text), "chat: %s sửa sổ thay đổi", chatID)
}

// RefreshDivergence updates a chat's divergence log now.
func (a *App) RefreshDivergence(chatID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := a.chat.RefreshDivergence(ctx, chatID)
	return out, act(err, "chat: %s cập nhật sổ thay đổi", chatID)
}

func (a *App) LogDir() string { return a.paths.Logs }

// PromptsDir is the folder of editable prompt files.
func (a *App) PromptsDir() string { return prompts.Dir() }

// OpenPromptsDir shows the prompt folder in Explorer.
func (a *App) OpenPromptsDir() error {
	return exec.Command("explorer", prompts.Dir()).Start()
}

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
