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
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"immersive/internal/llm"
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
}

type Engine struct {
	st      *store.Store
	rag     *rag.Store
	spaces  rag.Spaces
	resolve llm.Resolver
	opts    func() Options
	emit    func(event string, data any)

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

func New(st *store.Store, r *rag.Store, spaces rag.Spaces, resolve llm.Resolver, opts func() Options, emit func(string, any)) *Engine {
	return &Engine{st: st, rag: r, spaces: spaces, resolve: resolve, opts: opts, emit: emit, running: map[string]context.CancelFunc{}}
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
	if n := len(msgs); n > 1 && msgs[n-1].Role == "assistant" {
		if err := e.st.DeleteMessage(msgs[n-1].ID); err != nil {
			return err
		}
	}
	go e.respond(chatID)
	return nil
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
		if err := e.compress(context.Background(), chatID); err != nil {
			log.Printf("chat: compress memory: %v", err)
		}
	}
}

func (e *Engine) generate(ctx context.Context, chatID string) (string, error) {
	opts := e.opts()
	c, err := e.st.Chat(chatID)
	if err != nil {
		return "", err
	}
	var scen *store.Scenario
	if c.ScenarioID != "" {
		if s, err := e.st.Scenario(c.ScenarioID); err == nil {
			scen = &s
		}
	}
	var system string
	var kbIDs []string
	if c.Mode == store.ModeScene {
		if scen == nil {
			return "", fmt.Errorf("bối cảnh của scene này đã bị xoá")
		}
		cards, _ := e.st.Characters()
		system = buildSceneSystem(*scen, c, cards)
		kbIDs = union(scen.KBIDs, nil)
	} else {
		char, err := e.st.Character(c.CharacterID)
		if err != nil {
			return "", err
		}
		system = buildSystem(char, scen, c)
		kbIDs = char.KBIDs
		if scen != nil {
			kbIDs = union(kbIDs, scen.KBIDs)
		}
	}
	history, err := e.st.Messages(chatID, true)
	if err != nil {
		return "", err
	}
	provider, err := e.resolve(llm.RoleChat, c.ProviderID)
	if err != nil {
		return "", err
	}

	// Retrieval query: the latest exchange carries the most signal.
	var query string
	for i := len(history) - 1; i >= 0 && i >= len(history)-2; i-- {
		query = truncate(history[i].Content, 600) + "\n" + query
	}

	var memory string
	if query != "" {
		hits, err := e.rag.SearchKBs(ctx, e.spaces, query, rag.Filter{KBIDs: append(kbIDs, MemoryKB(chatID))}, opts.RAGK)
		if err != nil {
			log.Printf("chat: retrieval failed, continuing without memory: %v", err)
		}
		memory = rag.FormatContext(hits, opts.ContextChars)
	}

	var images []llm.Part
	if opts.AttachImages && provider.SupportsVision() && len(kbIDs) > 0 && query != "" {
		hits, err := e.rag.SearchImages(ctx, query, rag.Filter{KBIDs: kbIDs}, opts.ImageK)
		if err == nil {
			for _, h := range hits {
				if data, err := os.ReadFile(h.Text); err == nil {
					images = append(images, llm.Part{ImageB64: base64.StdEncoding.EncodeToString(data), ImageMIME: mime(h.Text)})
				}
			}
		}
	}

	msgs := make([]llm.Message, 0, len(history))
	for _, m := range history {
		role := llm.RoleUser
		if m.Role == "assistant" {
			role = llm.RoleAssistant
		}
		msgs = append(msgs, llm.TextMessage(role, m.Content))
	}
	// Attach retrieved context to the final user turn only (it is not persisted).
	if n := len(msgs); n > 0 && msgs[n-1].Role == llm.RoleUser && (memory != "" || len(images) > 0) {
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
	if len(msgs) == 0 || msgs[0].Role != llm.RoleUser {
		// Some APIs require the conversation to open with a user turn.
		msgs = append([]llm.Message{llm.TextMessage(llm.RoleUser, "*("+c.PersonaName+" bước vào câu chuyện)*")}, msgs...)
	}

	e.emit("chat:start", map[string]any{"chatId": chatID})
	return provider.Stream(ctx, llm.Request{
		Purpose:   "chat · " + c.Title,
		System:    system,
		Messages:  msgs,
		MaxTokens: opts.MaxTokens,
		OnReasoning: func(string) {
			e.emit("chat:thinking", map[string]any{"chatId": chatID})
		},
	}, func(delta string) {
		e.emit("chat:delta", map[string]any{"chatId": chatID, "delta": delta})
	})
}

const memoryPrompt = `Summarize this roleplay excerpt between %s and %s as long-term memory notes.
Capture: events that happened, facts revealed about either side, promises and plans, relationship and mood changes, items gained or lost, where the scene is now.
Write terse bullet points in the same language as the conversation. Output only the notes.

%s`

// compress folds the oldest active messages into a memory document once the
// live window grows past Options.Window.
func (e *Engine) compress(ctx context.Context, chatID string) error {
	opts := e.opts()
	msgs, err := e.st.Messages(chatID, true)
	if err != nil || len(msgs) <= opts.Window {
		return err
	}
	fold := msgs[:len(msgs)-opts.Window/2]
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
	p, err := e.resolve(llm.RoleSummarize, "")
	if err != nil {
		return err
	}
	notes, err := llm.Complete(ctx, p, llm.Request{Purpose: "nén ký ức · " + c.Title, MaxTokens: 6000, Messages: []llm.Message{
		llm.TextMessage(llm.RoleUser, fmt.Sprintf(memoryPrompt, other, c.PersonaName, sb.String()))}})
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
	sb.WriteString(`You are {{char}} in an immersive, ongoing roleplay with {{user}}.
- Stay in character at all times. Write only {{char}}'s words, actions and thoughts; never speak or act for {{user}}.
- Use *asterisks* for actions and narration, plain text for speech.
- A <memory> block may precede {{user}}'s message: it is knowledge retrieved from the source work and from earlier in this conversation. Treat it as canon and stay consistent with it; never mention the block itself. When it doesn't cover something, improvise in a way that fits the canon.
- Reply in the language {{user}} writes in.
- Keep replies vivid and focused (usually 1-4 paragraphs), move the scene forward, and leave room for {{user}} to act.

<character>
`)
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
	sb.WriteString(`You are the narrator and game master of an immersive, ongoing interactive story. The player has stepped into it as {{user}}.
- You run the world and every character except {{user}}: describe the scene, voice the cast, and play out the consequences of {{user}}'s actions honestly, including failure and danger.
- {{user}} belongs to the player alone. Never write {{user}}'s speech, actions, thoughts, feelings or decisions, and never assume their next move. End each turn at a moment that invites {{user}} to act.
- The player writes *actions* in asterisks and speech as plain text.
- Split every turn into blocks, each opened by a speaker tag alone on its own line: [Narrator] for narration and scene description, or [Name] for a character who speaks or acts. Inside a character's block, write their *actions* in asterisks and their speech in quotes. Use the exact names from the cast; a new character gets a short, consistent name of their own. Start a new block whenever the speaker changes, and never open a block for {{user}}. Example:
[Narrator]
Rain hammers the shrine roof as the lanterns gutter out.
[Elder Mo]
*grips his staff* "Someone is coming. Stay behind me."
- Each cast member has their own goals, knowledge and voice, and acts on them without waiting for {{user}}. Keep them true to the source: personality, abilities, relationships and the way they talk. A character only knows what they would plausibly know at this point of the story.
- If {{user}} is a character from the source, everyone treats them as that character (same relationships, history, reputation), but the player decides what they do, and the story may diverge from canon because of it.
- Characters not listed in the cast may appear when the story calls for it, as long as they fit the source.
- A <memory> block may precede the player's message: knowledge retrieved from the source work and from earlier in this story. Treat it as canon and stay consistent with it; never mention the block itself. When it doesn't cover something, improvise in a way that fits the canon.
- Reply in the language the player writes in. Keep the tags exactly as above ([Narrator], [Name]) whatever the language. Keep turns vivid and focused (usually 2-6 blocks) and keep the story moving.

`)
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
