// Package chat runs roleplay conversations.
//
// Per turn: retrieve relevant lore + past-conversation memory from the vector
// store, attach it to the newest user turn (not the system prompt, which stays
// byte-stable and cacheable), stream the reply, and periodically fold old
// messages into long-term memory so the live context stays small.
package chat

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"immersive/internal/llm"
	"immersive/internal/lore"
	"immersive/internal/prompts"
	"immersive/internal/rag"
	"immersive/internal/store"
)

type Options struct {
	RAGK         int  // text hits per turn
	ContextChars int  // max chars of retrieved memory per turn
	Window       int  // active messages before old ones get folded into memory
	MaxTokens    int  // reply length cap
	AttachImages bool // attach retrieved comic pages when the model supports vision
	ImageK       int
	// DivergenceEvery is how many replies pass between updates of the
	// divergence log (0 = never).
	DivergenceEvery int
}

// vectors is the part of the vector store chat uses (rag.Store; swapped in tests).
type vectors interface {
	SearchKBs(ctx context.Context, spaces rag.Spaces, query string, f rag.Filter, k int) ([]rag.Hit, error)
	SearchImages(ctx context.Context, query string, f rag.Filter, k int) ([]rag.Hit, error)
	AddTexts(ctx context.Context, sp rag.Space, docs []rag.Doc) error
	Delete(ctx context.Context, f rag.Filter) error
}

type Engine struct {
	st      *store.Store
	rag     vectors
	lore    *lore.Lore
	spaces  rag.Spaces
	resolve llm.Resolver
	opts    func() Options
	emit    func(event string, data any)

	mu        sync.Mutex
	running   map[string]context.CancelFunc
	diverging map[string]bool // chats whose divergence log is being updated
}

func New(st *store.Store, r vectors, lr *lore.Lore, spaces rag.Spaces, resolve llm.Resolver, opts func() Options, emit func(string, any)) *Engine {
	return &Engine{st: st, rag: r, lore: lr, spaces: spaces, resolve: resolve, opts: opts, emit: emit,
		running: map[string]context.CancelFunc{}, diverging: map[string]bool{}}
}

func MemoryKB(chatID string) string { return "chat:" + chatID }

// Narrator is the display name of the model's side in scene mode.
const Narrator = "Người dẫn truyện"

// Create starts a conversation and posts the opening/greeting message.
func (e *Engine) Create(c *store.Chat) error {
	if c.PersonaName == "" {
		c.PersonaName = "Bạn"
	}
	if c.Mode == store.ModeScene {
		return e.createScene(c)
	}
	char, err := e.st.Character(c.CharacterID)
	if err != nil {
		return err
	}
	if c.Title == "" {
		c.Title = char.Name
	}
	if err := e.st.SaveChat(c); err != nil {
		return err
	}
	var opening []string
	if c.ScenarioID != "" {
		if sc, err := e.st.Scenario(c.ScenarioID); err == nil && sc.Opening != "" {
			opening = append(opening, sc.Opening)
		}
	}
	if char.Greeting != "" {
		opening = append(opening, char.Greeting)
	}
	if len(opening) > 0 {
		_, err = e.st.AddMessage(c.ID, "assistant", fill(strings.Join(opening, "\n\n"), char.Name, c.PersonaName))
	}
	return err
}

// createScene starts a scene: the player takes a role, the narrator opens.
func (e *Engine) createScene(c *store.Chat) error {
	sc, err := e.st.Scenario(c.ScenarioID)
	if err != nil {
		return fmt.Errorf("scene cần một bối cảnh: %w", err)
	}
	c.CharacterID = ""
	if c.Title == "" {
		c.Title = sc.Title + " · " + c.PersonaName
	}
	if err := e.st.SaveChat(c); err != nil {
		return err
	}
	if sc.Opening != "" {
		_, err = e.st.AddMessage(c.ID, "assistant", fill(sc.Opening, Narrator, c.PersonaName))
	}
	return err
}

func (e *Engine) Delete(ctx context.Context, chatID string) error {
	e.Stop(chatID)
	if err := e.rag.Delete(ctx, rag.Filter{KBIDs: []string{MemoryKB(chatID)}}); err != nil {
		log.Printf("chat: delete memory: %v", err)
	}
	return e.st.DeleteChat(chatID)
}

func (e *Engine) Busy(chatID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[chatID] != nil
}

func (e *Engine) Stop(chatID string) {
	e.mu.Lock()
	if c := e.running[chatID]; c != nil {
		c()
	}
	e.mu.Unlock()
}

// Send stores the user message and generates the reply in the background.
func (e *Engine) Send(chatID, text string) (store.Message, error) {
	if e.Busy(chatID) {
		return store.Message{}, fmt.Errorf("nhân vật đang trả lời")
	}
	m, err := e.st.AddMessage(chatID, "user", text)
	if err != nil {
		return m, err
	}
	go e.respond(chatID)
	return m, nil
}

// Regenerate drops the last reply and generates a new one.
func (e *Engine) Regenerate(chatID string) error {
	if e.Busy(chatID) {
		return fmt.Errorf("nhân vật đang trả lời")
	}
	msgs, err := e.st.Messages(chatID, true)
	if err != nil {
		return err
	}
	// A written greeting or opening stays; a narrator opening the model wrote can be redone.
	if n := len(msgs); n > 0 && msgs[n-1].Role == "assistant" && (n > 1 || e.generatedOpening(chatID)) {
		if err := e.st.DeleteMessage(msgs[n-1].ID); err != nil {
			return err
		}
	}
	go e.respond(chatID)
	return nil
}

// Begin has the narrator open a scene that has no opening text yet.
func (e *Engine) Begin(chatID string) error {
	if msgs, err := e.st.Messages(chatID, true); err != nil || len(msgs) > 0 || e.Busy(chatID) {
		return err
	}
	go e.respond(chatID)
	return nil
}

// generatedOpening reports whether a scene's first message came from the model
// (its scenario has no written opening).
func (e *Engine) generatedOpening(chatID string) bool {
	c, err := e.st.Chat(chatID)
	if err != nil || c.Mode != store.ModeScene {
		return false
	}
	sc, err := e.st.Scenario(c.ScenarioID)
	return err == nil && sc.Opening == ""
}

func (e *Engine) respond(chatID string) {
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.running[chatID] = cancel
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		delete(e.running, chatID)
		e.mu.Unlock()
	}()

	reply, err := e.generate(ctx, chatID)
	if strings.TrimSpace(reply) != "" {
		m, saveErr := e.st.AddMessage(chatID, "assistant", strings.TrimSpace(reply))
		if saveErr == nil {
			e.emit("chat:done", map[string]any{"chatId": chatID, "message": m})
		}
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("chat: %s lỗi: %v", chatID, err)
		e.emit("chat:error", map[string]any{"chatId": chatID, "error": err.Error()})
	} else if strings.TrimSpace(reply) == "" {
		e.emit("chat:done", map[string]any{"chatId": chatID})
	}
	if err == nil {
		go e.updateDivergence(chatID)
		if err := e.compress(context.Background(), chatID); err != nil {
			log.Printf("chat: compress memory: %v", err)
		}
	}
}

// maxLookups is how many tool calls the model may make before one reply.
const maxLookups = 4

// setup is what a turn of a conversation needs, whatever the model.
type setup struct {
	chat    store.Chat
	scen    *store.Scenario
	system  string   // rules, cards, scenario (byte-stable for caching)
	kbIDs   []string // the works the story draws from
	other   string   // who the model plays: the character, or the narrator
	history []store.Message
}

func (e *Engine) setup(chatID string) (setup, error) {
	var s setup
	c, err := e.st.Chat(chatID)
	if err != nil {
		return s, err
	}
	s.chat = c
	if c.ScenarioID != "" {
		if sc, err := e.st.Scenario(c.ScenarioID); err == nil {
			s.scen = &sc
		}
	}
	if c.Mode == store.ModeScene {
		if s.scen == nil {
			return s, fmt.Errorf("bối cảnh của scene này đã bị xoá")
		}
		cards, _ := e.st.Characters()
		s.system = buildSceneSystem(*s.scen, c, cards)
		s.kbIDs = union(s.scen.KBIDs, nil)
		s.other = Narrator
	} else {
		char, err := e.st.Character(c.CharacterID)
		if err != nil {
			return s, err
		}
		s.system = buildSystem(char, s.scen, c)
		s.kbIDs = char.KBIDs
		if s.scen != nil {
			s.kbIDs = union(s.kbIDs, s.scen.KBIDs)
		}
		s.other = char.Name
	}
	s.history, err = e.st.Messages(chatID, true)
	return s, err
}

// loreBlock tells the model how to use the source: the rules, the story map
// (in at most mapChars) and this story's divergence log.
func (e *Engine) loreBlock(s setup, mapChars int) string {
	var sb strings.Builder
	sb.WriteString("\n" + fill(prompts.Get("chat-lore"), s.other, s.chat.PersonaName) + "\n")
	if m := e.lore.Map(s.kbIDs, mapChars); m != "" {
		sb.WriteString("\n<story_map>\n" + m + "\n</story_map>\n")
	}
	if d := strings.TrimSpace(s.chat.Divergence); d != "" {
		sb.WriteString("\n<divergence>\n" + d + "\n</divergence>\n")
	}
	return sb.String()
}

func (e *Engine) generate(ctx context.Context, chatID string) (string, error) {
	opts := e.opts()
	s, err := e.setup(chatID)
	if err != nil {
		return "", err
	}
	c := s.chat
	provider, err := e.resolve(llm.RoleChat, c.ProviderID)
	if err != nil {
		return "", err
	}
	maxOut, inChars := llm.Budget(provider, opts.MaxTokens)
	system := s.system + e.loreBlock(s, inChars/5)

	// Retrieval query: the latest exchange carries the most signal. Before
	// anything has been said, look up where the story starts and who the player is.
	var query string
	for i := len(s.history) - 1; i >= 0 && i >= len(s.history)-2; i-- {
		query = truncate(s.history[i].Content, 600) + "\n" + query
	}
	if len(s.history) == 0 && s.scen != nil {
		query = strings.TrimSpace(s.scen.Title + "\n" + truncate(s.scen.Setting, 600) + "\n" + c.PersonaName + " " + truncate(c.PersonaDesc, 300))
	}
	memoryChars := min(opts.ContextChars, inChars/4)
	e.emit("chat:start", map[string]any{"chatId": chatID})
	onDelta := func(delta string) {
		e.emit("chat:delta", map[string]any{"chatId": chatID, "delta": delta})
	}
	onReasoning := func(string) {
		e.emit("chat:thinking", map[string]any{"chatId": chatID})
	}

	// With tools, the model looks the source up itself; only this
	// conversation's own memory is retrieved for it.
	if tu, ok := provider.(llm.ToolUser); ok && len(s.kbIDs) > 0 {
		memory := e.retrieve(ctx, query, []string{MemoryKB(chatID)}, opts.RAGK/2+1, memoryChars)
		// Lookup results need room too: a quarter of the context.
		msgs := e.messages(s, memory, nil, inChars-len([]rune(system))-inChars/4)
		reply, err := e.withTools(ctx, tu, chatID, s.kbIDs, llm.Request{Purpose: "chat · " + c.Title, System: system,
			Messages: msgs, MaxTokens: maxOut, OnReasoning: onReasoning}, max(inChars/(4*maxLookups), 1000), onDelta)
		if !errors.Is(err, llm.ErrNoTools) {
			return reply, err
		}
		log.Printf("chat: %s: model không gọi được công cụ, tự truy xuất thay", c.Title)
	}

	memory := e.retrieve(ctx, query, append(slices.Clone(s.kbIDs), MemoryKB(chatID)), opts.RAGK, memoryChars)
	var images []llm.Part
	if opts.AttachImages && provider.SupportsVision() && len(s.kbIDs) > 0 && query != "" {
		hits, err := e.rag.SearchImages(ctx, query, rag.Filter{KBIDs: s.kbIDs}, opts.ImageK)
		if err == nil {
			for _, h := range hits {
				if data, err := os.ReadFile(h.Text); err == nil {
					images = append(images, llm.Part{ImageB64: base64.StdEncoding.EncodeToString(data), ImageMIME: mime(h.Text)})
				}
			}
		}
	}
	msgs := e.messages(s, memory, images, inChars-len([]rune(system)))
	return provider.Stream(ctx, llm.Request{Purpose: "chat · " + c.Title, System: system, Messages: msgs,
		MaxTokens: maxOut, OnReasoning: onReasoning}, onDelta)
}

// retrieve searches kbIDs for the query and formats what it finds.
func (e *Engine) retrieve(ctx context.Context, query string, kbIDs []string, k, maxChars int) string {
	if query == "" {
		return ""
	}
	hits, err := e.rag.SearchKBs(ctx, e.spaces, query, rag.Filter{KBIDs: kbIDs}, k)
	if err != nil {
		log.Printf("chat: retrieval failed, continuing without memory: %v", err)
	}
	return rag.FormatContext(hits, maxChars)
}

// messages turns the history into model messages that fit in maxChars
// (oldest dropped first) and attaches retrieved memory and pages to the
// newest user turn (not persisted; the system prompt stays stable).
func (e *Engine) messages(s setup, memory string, images []llm.Part, maxChars int) []llm.Message {
	maxChars -= len([]rune(memory)) + 1500*len(images)
	history := s.history
	total := 0
	for i := len(history) - 1; i >= 0; i-- {
		total += len([]rune(history[i].Content)) + 20
		if total > maxChars && i < len(history)-1 {
			log.Printf("chat: %s: bỏ %d tin nhắn cũ cho vừa ngữ cảnh của model", s.chat.Title, i+1)
			history = history[i+1:]
			break
		}
	}
	msgs := make([]llm.Message, 0, len(history)+1)
	for _, m := range history {
		role := llm.RoleUser
		if m.Role == "assistant" {
			role = llm.RoleAssistant
		}
		msgs = append(msgs, llm.TextMessage(role, m.Content))
	}
	if len(msgs) == 0 || msgs[0].Role != llm.RoleUser {
		// Some APIs require the conversation to open with a user turn.
		msgs = append([]llm.Message{llm.TextMessage(llm.RoleUser, "*("+s.chat.PersonaName+" bước vào câu chuyện)*")}, msgs...)
	}
	if n := len(msgs); msgs[n-1].Role == llm.RoleUser && (memory != "" || len(images) > 0) {
		var parts []llm.Part
		if memory != "" {
			parts = append(parts, llm.Part{Text: "<memory>\n" + memory + "\n</memory>"})
		}
		if len(images) > 0 {
			parts = append(parts, llm.Part{Text: "<reference_pages>Các trang truyện liên quan:</reference_pages>"})
			parts = append(parts, images...)
		}
		msgs[n-1].Parts = append(parts, msgs[n-1].Parts...)
	}
	return msgs
}

// withTools runs a reply in which the model may look the source up first. It
// returns llm.ErrNoTools, before anything is streamed, when the model can't
// call tools.
func (e *Engine) withTools(ctx context.Context, tu llm.ToolUser, chatID string, kbIDs []string, req llm.Request, resultChars int, onDelta func(string)) (string, error) {
	var reply strings.Builder
	purpose := req.Purpose
	lookups := 0
	for round := 1; ; round++ {
		req.Tools = lore.Tools
		req.NoMoreTools = lookups >= maxLookups
		if round > 1 {
			req.Purpose = fmt.Sprintf("%s · lượt %d", purpose, round)
		}
		first := true
		t, err := tu.Turn(ctx, req, func(d string) {
			if first && reply.Len() > 0 {
				onDelta("\n\n") // text before a lookup and after it are paragraphs of one reply
			}
			first = false
			onDelta(d)
		})
		if t.Text != "" {
			if reply.Len() > 0 {
				reply.WriteString("\n\n")
			}
			reply.WriteString(t.Text)
		}
		if err != nil || len(t.Calls) == 0 || req.NoMoreTools {
			return reply.String(), err
		}
		msg := llm.Message{Role: llm.RoleAssistant, Calls: t.Calls}
		if strings.TrimSpace(t.Text) != "" {
			msg.Parts = []llm.Part{{Text: t.Text}}
		}
		req.Messages = append(req.Messages, msg)
		for _, call := range t.Calls {
			result := "Đã hết lượt tra cứu. Hãy viết câu trả lời từ những gì đã có."
			if lookups < maxLookups {
				lookups++
				e.emit("chat:lookup", map[string]any{"chatId": chatID, "tool": call.Name, "args": call.Args})
				result = e.lore.Run(ctx, kbIDs, call, resultChars)
				log.Printf("chat: %s tra cứu %s %s → %d ký tự", chatID, call.Name, call.Args, len([]rune(result)))
			}
			req.Messages = append(req.Messages, llm.Message{Role: llm.RoleTool, CallID: call.ID, Parts: []llm.Part{{Text: result}}})
		}
	}
}

// compress folds the oldest active messages into a memory document once the
// live window grows past Options.Window.
func (e *Engine) compress(ctx context.Context, chatID string) error {
	opts := e.opts()
	msgs, err := e.st.Messages(chatID, true)
	if err != nil || len(msgs) <= opts.Window {
		return err
	}
	fold := msgs[:len(msgs)-opts.Window/2]
	p, err := e.resolve(llm.RoleSummarize, "")
	if err != nil {
		return err
	}
	maxOut, inChars := llm.Budget(p, 3000)
	fold = fitMessages(fold, inChars-2000) // the rest folds next time
	c, err := e.st.Chat(chatID)
	if err != nil {
		return err
	}
	other := Narrator
	if c.Mode != store.ModeScene {
		char, err := e.st.Character(c.CharacterID)
		if err != nil {
			return err
		}
		other = char.Name
	}
	var sb strings.Builder
	for _, m := range fold {
		who := c.PersonaName
		if m.Role == "assistant" {
			who = other
		}
		fmt.Fprintf(&sb, "%s: %s\n\n", who, m.Content)
	}
	notes, err := llm.Complete(ctx, p, llm.Request{Purpose: "nén ký ức · " + c.Title, MaxTokens: maxOut, Messages: []llm.Message{
		llm.TextMessage(llm.RoleUser, prompts.Get("chat-memory", "other", other, "player", c.PersonaName, "excerpt", sb.String()))}})
	if err != nil {
		return err
	}
	last := fold[len(fold)-1].ID
	// Conversation memory always lives in the local space (see rag.Spaces).
	err = e.rag.AddTexts(ctx, rag.Space{}, []rag.Doc{{
		ID:       fmt.Sprintf("%s:mem:%d", chatID, last),
		Text:     strings.TrimSpace(notes),
		Metadata: map[string]any{"kb_id": MemoryKB(chatID), "source_id": MemoryKB(chatID), "level": rag.LevelMemory},
	}})
	if err != nil {
		return err
	}
	return e.st.MarkSummarized(chatID, last)
}

func buildSystem(c store.Character, scen *store.Scenario, chat store.Chat) string {
	var sb strings.Builder
	sb.WriteString(prompts.Get("chat-character") + "\n\n<character>\n")
	field := func(label, v string) {
		if strings.TrimSpace(v) != "" {
			fmt.Fprintf(&sb, "%s: %s\n", label, strings.TrimSpace(v))
		}
	}
	field("Name", c.Name)
	field("Description", c.Description)
	field("Personality", c.Personality)
	field("Speaking style", c.SpeakingStyle)
	if c.ExampleDialogue != "" {
		sb.WriteString("Example dialogue:\n" + strings.TrimSpace(c.ExampleDialogue) + "\n")
	}
	sb.WriteString("</character>\n")
	if scen != nil {
		fmt.Fprintf(&sb, "\n<scenario title=%q>\n%s\n</scenario>\n", scen.Title, strings.TrimSpace(scen.Setting))
	}
	if chat.PersonaDesc != "" {
		fmt.Fprintf(&sb, "\n<user_persona>\n%s\n</user_persona>\n", strings.TrimSpace(chat.PersonaDesc))
	}
	return fill(sb.String(), c.Name, chat.PersonaName)
}

// buildSceneSystem makes the model the narrator of a scene in which the player
// plays {{user}}. Cast members that have a saved character card with the same
// name get the card's personality and speaking style too.
func buildSceneSystem(scen store.Scenario, chat store.Chat, cards []store.Character) string {
	var sb strings.Builder
	sb.WriteString(prompts.Get("chat-narrator") + "\n\n")
	fmt.Fprintf(&sb, "<scenario title=%q>\n%s\n</scenario>\n", scen.Title, strings.TrimSpace(scen.Setting))

	byName := map[string]store.Character{}
	for _, c := range cards {
		byName[strings.ToLower(strings.TrimSpace(c.Name))] = c
	}
	player := strings.ToLower(strings.TrimSpace(chat.PersonaName))
	var cast strings.Builder
	for _, m := range scen.Cast {
		key := strings.ToLower(strings.TrimSpace(m.Name))
		if key == "" || key == player {
			continue // the player's own role is described below
		}
		fmt.Fprintf(&cast, "- %s: %s\n", strings.TrimSpace(m.Name), strings.TrimSpace(m.Description))
		if card, ok := byName[key]; ok {
			if card.Personality != "" {
				fmt.Fprintf(&cast, "  Personality: %s\n", strings.TrimSpace(card.Personality))
			}
			if card.SpeakingStyle != "" {
				fmt.Fprintf(&cast, "  Speaking style: %s\n", strings.TrimSpace(card.SpeakingStyle))
			}
		}
	}
	if cast.Len() > 0 {
		sb.WriteString("\n<cast>\n" + cast.String() + "</cast>\n")
	}

	fmt.Fprintf(&sb, "\n<player_character name=%q>\n", chat.PersonaName)
	if d := strings.TrimSpace(chat.PersonaDesc); d != "" {
		sb.WriteString(d + "\n")
	} else {
		sb.WriteString("A newcomer to this story; the player reveals who they are through play.\n")
	}
	sb.WriteString("</player_character>\n")
	return fill(sb.String(), Narrator, chat.PersonaName)
}

func fill(s, char, user string) string {
	return strings.NewReplacer("{{char}}", char, "{{user}}", user).Replace(s)
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string{}, a...), b...) {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// truncate keeps the last n runes: the end of a long message is usually the most relevant.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func mime(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	}
	return "image/png"
}
