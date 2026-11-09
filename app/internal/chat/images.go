package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"immersive/internal/llm"
	"immersive/internal/lore"
	"immersive/internal/prompts"
	"immersive/internal/store"
)

// Imaging is how conversations get illustrated. Pictures can be drawn when
// Available says so (an image provider is set): from the "Tạo ảnh" button on
// any message, and, when Auto is on too, by the model itself with the
// generate_image tool. A nil func counts as yes.
type Imaging struct {
	Imager    func() (llm.Imager, error)
	Available func() bool
	Auto      func() bool
	Dir       string // <Dir>/<chat id>/<file> holds the pictures drawn in a chat
}

// SetImaging turns illustrations on (call before the first chat).
func (e *Engine) SetImaging(im Imaging) { e.img = im }

// canDraw: an image provider is set.
func (e *Engine) canDraw() bool {
	return e.img.Imager != nil && (e.img.Available == nil || e.img.Available())
}

// imagesOn: the model is offered the drawing tool in replies.
func (e *Engine) imagesOn() bool {
	return e.canDraw() && (e.img.Auto == nil || e.img.Auto())
}

// ImageTool lets the model illustrate the scene.
var ImageTool = llm.Tool{
	Name: "generate_image",
	Description: "Draw an illustration of the scene and show it to the player. Write `prompt` in English as a detailed picture description: " +
		"who is in the frame and how each looks (face, hair, eyes, build, clothes), their poses and expressions, what is happening, " +
		"the place, time of day, lighting, camera angle and framing. List everyone in the frame in `characters`: their reference pictures " +
		"are attached so they look like themselves. Add `references` (chapter and page numbers from read_chapter) for pages of the source " +
		"that show the place, an object or a character best. At most one picture per reply.",
	Params: map[string]any{
		"prompt":     map[string]any{"type": "string", "description": "Detailed visual description of the picture, in English."},
		"characters": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Names of the characters in the frame."},
		"references": map[string]any{"type": "array", "description": "Pages of the source to follow.", "items": map[string]any{"type": "object",
			"properties": map[string]any{"chapter": map[string]any{"type": "integer"}, "page": map[string]any{"type": "integer"}, "work": map[string]any{"type": "string"}}}},
		"continue_previous": map[string]any{"type": "boolean", "description": "True when this picture follows on from the previous illustration (same place, same moment)."},
		"aspect":            map[string]any{"type": "string", "enum": []string{"landscape", "portrait", "square"}},
	},
	Required: []string{"prompt"},
}

// An illustration sits in a reply as its own line: [Image: <chat>/<file> | caption].
var imageLine = regexp.MustCompile(`(?m)^\[Image: ([^|\]\n]+?)(?: \| ([^\]\n]*))?\]$`)

// speakerTag is a "[Name]" line of a scene reply.
var speakerTag = regexp.MustCompile(`(?m)^\s*\*{0,2}\[([^\[\]\n:|]{1,40})\]`)

func imageMarker(rel, caption string) string {
	caption = strings.NewReplacer("]", ")", "[", "(", "\n", " ", "|", "/").Replace(shorten(caption, 160))
	return "[Image: " + rel + " | " + caption + "]"
}

// forModel is a message as the model reads it: illustrations become a note
// of what was shown (the model can't see them, and must not imitate the marker).
func forModel(content string) string {
	return imageLine.ReplaceAllString(content, "(An illustration was shown to the player here: $2)")
}

// lastImage is the newest illustration in the history.
func lastImage(history []store.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if m := imageLine.FindAllStringSubmatch(history[i].Content, -1); len(m) > 0 {
			return m[len(m)-1][1]
		}
	}
	return ""
}

// ImagePath is where a picture of a chat (as named in its marker) is, or an
// error when the name points elsewhere.
func (e *Engine) ImagePath(rel string) (string, error) {
	if e.img.Dir == "" {
		return "", errors.New("chưa bật tạo ảnh")
	}
	p := filepath.Join(e.img.Dir, filepath.FromSlash(rel))
	if r, err := filepath.Rel(e.img.Dir, p); err != nil || strings.HasPrefix(r, "..") {
		return "", errors.New("đường dẫn ảnh không hợp lệ")
	}
	return p, nil
}

type drawArgs struct {
	Prompt     string   `json:"prompt"`
	Characters []string `json:"characters"`
	References []struct {
		Chapter json.RawMessage `json:"chapter"`
		Page    json.RawMessage `json:"page"`
		Work    string          `json:"work"`
	} `json:"references"`
	Continue bool   `json:"continue_previous"`
	Aspect   string `json:"aspect"`
}

// draw runs a generate_image call from the model. It returns the marker to
// put in the reply ("" when nothing was drawn) and what to tell the model.
func (e *Engine) draw(ctx context.Context, s setup, raw string) (marker, result string) {
	var a drawArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil || strings.TrimSpace(a.Prompt) == "" {
		return "", `Error: call generate_image with {"prompt": "...", "characters": [...]}.`
	}
	chatID := s.chat.ID
	e.emit("chat:image", map[string]any{"chatId": chatID, "status": "drawing"})
	rel, used, missing, err := e.drawPicture(ctx, s, a)
	if err != nil {
		e.emit("chat:image", map[string]any{"chatId": chatID, "status": "error", "error": err.Error()})
		return "", "Drawing failed (" + err.Error() + "). Go on with the reply without a picture and don't mention it."
	}
	e.emit("chat:image", map[string]any{"chatId": chatID, "status": "done"})
	result = "Done: the picture is now shown to the player."
	if len(used) > 0 {
		result += " References attached: " + strings.Join(used, "; ") + "."
	}
	if len(missing) > 0 {
		result += " No reference picture was found for: " + strings.Join(missing, ", ") + " (drawn from the description only)."
	}
	result += " Go on with the reply in text; don't describe the picture or mention that you drew it."
	return imageMarker(rel, a.Prompt), result
}

// drawPicture draws a picture with its references and saves it; rel names it
// for the marker.
func (e *Engine) drawPicture(ctx context.Context, s setup, a drawArgs) (rel string, used, missing []string, err error) {
	chatID := s.chat.ID
	imager, err := e.img.Imager()
	if err != nil {
		return "", nil, nil, err
	}
	refs, used, missing := e.references(ctx, s, a)
	prompt := strings.TrimSpace(a.Prompt)
	if style := strings.TrimSpace(e.opts().ImageStyle); style != "" {
		prompt += "\n\nArt style: " + style
	}
	start := time.Now()
	img, err := imager.Draw(ctx, llm.ImageRequest{Prompt: prompt, Refs: refs, Aspect: a.Aspect,
		Continue: a.Continue && len(used) > 0 && strings.HasPrefix(used[0], "the previous illustration"), Purpose: "minh hoạ · " + s.chat.Title})
	if err != nil {
		log.Printf("chat: %s vẽ minh hoạ thất bại: %v", chatID, err)
		return "", used, missing, err
	}
	data, err := base64.StdEncoding.DecodeString(img.B64)
	if err != nil {
		return "", used, missing, fmt.Errorf("ảnh trả về bị hỏng: %w", err)
	}
	ext := ".png"
	switch img.MIME {
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	}
	rel = chatID + "/" + strconv.FormatInt(time.Now().UnixNano(), 36) + ext
	path, _ := e.ImagePath(rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", used, missing, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", used, missing, err
	}
	log.Printf("chat: %s đã vẽ minh hoạ (%d ảnh tham chiếu: %s; %s) → %s", chatID, len(refs), strings.Join(used, "; "), time.Since(start).Round(time.Second), rel)
	return rel, used, missing, nil
}

// references gathers the pictures a drawing should follow, at most
// Imaging.MaxRefs: the previous illustration when the scene carries on from
// it, then each character (their card's picture, else the source page that
// shows them most), then the pages the model asked for.
func (e *Engine) references(ctx context.Context, s setup, a drawArgs) (refs []llm.RefImage, used, missing []string) {
	limit := e.opts().ImageMaxRefs
	if limit <= 0 {
		limit = 4
	}
	add := func(path, dataURI, label, desc string) bool {
		if len(refs) >= limit {
			return false
		}
		var ref llm.RefImage
		if dataURI != "" {
			head, b64, ok := strings.Cut(dataURI, ",")
			if !ok || !strings.HasSuffix(head, ";base64") {
				return false
			}
			ref = llm.RefImage{B64: b64, MIME: strings.TrimSuffix(strings.TrimPrefix(head, "data:"), ";base64")}
		} else {
			data, err := os.ReadFile(path)
			if err != nil {
				return false
			}
			ref = llm.RefImage{B64: base64.StdEncoding.EncodeToString(data), MIME: mime(path)}
		}
		ref.Label = label
		refs = append(refs, ref)
		used = append(used, desc)
		return true
	}

	if a.Continue {
		if rel := lastImage(s.history); rel != "" {
			if p, err := e.ImagePath(rel); err == nil {
				add(p, "", "the previous illustration of this story: keep the place, lighting and characters consistent with it", "the previous illustration")
			}
		}
	}

	cards, _ := e.st.Characters()
	if s.card != nil {
		cards = append([]store.Character{*s.card}, cards...)
	}
	player := strings.TrimSpace(s.chat.PersonaName)
	for _, name := range a.Characters {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		label := name + ": how this character looks"
		found := false
		for _, c := range cards {
			if c.Avatar != "" && strings.EqualFold(strings.TrimSpace(c.Name), name) {
				found = add("", c.Avatar, label, name+" (character card picture)")
				break
			}
		}
		if !found && len(s.kbIDs) > 0 {
			if pic, ok := e.lore.CharacterPicture(ctx, s.kbIDs, name); ok {
				found = add(pic.Path, "", label+" (a page of the source where they appear)", name+" ("+pic.Label+")")
			}
		}
		if !found && !strings.EqualFold(name, player) {
			missing = append(missing, name)
		}
	}

	for _, r := range a.References {
		ch, page := looseNumber(r.Chapter), looseNumber(r.Page)
		pic, err := e.lore.ChapterPicture(s.kbIDs, r.Work, ch, page)
		if err != nil {
			missing = append(missing, fmt.Sprintf("chapter %d page %d (%v)", ch, page, err))
			continue
		}
		add(pic.Path, "", "a page of the source to follow for the place and look ("+pic.Label+")", pic.Label)
	}
	return refs, used, missing
}

func looseNumber(raw json.RawMessage) int {
	n, _ := strconv.Atoi(strings.Trim(strings.TrimSpace(string(raw)), `"`))
	if n == 0 {
		if f, err := strconv.ParseFloat(strings.Trim(string(raw), `" `), 64); err == nil {
			n = int(f)
		}
	}
	return n
}

// maxPromptLookups is how many lookups the model may make while writing a
// picture's description.
const maxPromptLookups = 3

// Illustrate draws the moment of message msgID (the player's or a reply) and
// adds the picture to it: the "Tạo ảnh" button. The chat model reads the
// scene (and may look the source up), writes the picture's description and
// names who is in it; the app attaches their reference pictures; the image
// model draws. Progress comes as chat:illustrating, then chat:illustrated or
// chat:illustrate-error.
func (e *Engine) Illustrate(chatID string, msgID int64) error {
	if !e.canDraw() {
		return errors.New("chưa chọn provider ảnh — vào Cài đặt › Tạo ảnh minh hoạ")
	}
	e.mu.Lock()
	if e.drawing[chatID] {
		e.mu.Unlock()
		return errors.New("đang vẽ một ảnh khác của cuộc trò chuyện này — đợi xong đã")
	}
	e.drawing[chatID] = true
	e.mu.Unlock()
	go func() {
		defer func() { e.mu.Lock(); delete(e.drawing, chatID); e.mu.Unlock() }()
		if err := e.illustrate(context.Background(), chatID, msgID); err != nil {
			log.Printf("chat: %s tạo ảnh cho tin nhắn %d: %v", chatID, msgID, err)
			e.emit("chat:illustrate-error", map[string]any{"chatId": chatID, "msgId": msgID, "error": err.Error()})
		}
	}()
	return nil
}

func (e *Engine) illustrate(ctx context.Context, chatID string, msgID int64) error {
	s, err := e.setup(chatID)
	if err != nil {
		return err
	}
	var upto []store.Message
	for i, m := range s.history {
		if m.ID == msgID {
			upto = s.history[:i+1]
			break
		}
	}
	if upto == nil {
		return errors.New("không tìm thấy tin nhắn (có thể đã được gộp vào ký ức)")
	}
	stage := func(name string) {
		e.emit("chat:illustrating", map[string]any{"chatId": chatID, "msgId": msgID, "stage": name})
	}
	stage("writing")
	provider, err := e.resolve(llm.RoleChat, s.chat.ProviderID)
	if err != nil {
		return err
	}
	maxOut, inChars := llm.Budget(provider, 1500)
	system := e.system(ctx, s, inChars/12) + e.loreBlock(s, inChars/6)
	recent := slices.Clone(upto[max(0, len(upto)-6):])
	for i := range recent {
		recent[i].Content = forModel(recent[i].Content)
	}
	recent = fitLast(recent, inChars-len([]rune(system))-inChars/3)
	text := excerpt(recent, s)
	var profiles string
	if len(s.kbIDs) > 0 {
		profiles = e.presentProfiles(ctx, setup{chat: s.chat, card: s.card, scen: s.scen, kbIDs: s.kbIDs, history: recent}, inChars/8)
	}
	req := llm.Request{Purpose: "viết prompt ảnh · " + s.chat.Title, System: system, MaxTokens: maxOut,
		Messages: []llm.Message{llm.TextMessage(llm.RoleUser, prompts.Get("image-prompt", "excerpt", text, "profiles", profiles,
			"player", s.chat.PersonaName, "pictures", e.pictureList(ctx, s, text)))}}
	out, err := e.promptWithLookups(ctx, provider, s, req, inChars/8)
	if err != nil {
		return err
	}
	var a drawArgs
	if raw := jsonObject(out); raw == "" || json.Unmarshal([]byte(raw), &a) != nil || strings.TrimSpace(a.Prompt) == "" {
		return fmt.Errorf("model chat không viết được mô tả ảnh (không trả về JSON hợp lệ). Nó trả lời: %q", shorten(strings.TrimSpace(out), 400))
	}

	stage("drawing")
	s.history = upto
	rel, used, _, err := e.drawPicture(ctx, s, a)
	if err != nil {
		return err
	}
	cur, err := e.st.Messages(chatID, false)
	if err != nil {
		return err
	}
	for _, m := range cur {
		if m.ID == msgID {
			content := strings.TrimRight(m.Content, "\n") + "\n\n" + imageMarker(rel, a.Prompt)
			if err := e.st.UpdateMessage(msgID, content); err != nil {
				return err
			}
			e.emit("chat:illustrated", map[string]any{"chatId": chatID, "msgId": msgID, "content": content, "refs": used})
			return nil
		}
	}
	return errors.New("tin nhắn đã bị xoá trong lúc vẽ")
}

// promptWithLookups has the model answer req, looking the source up first
// when it can call tools.
func (e *Engine) promptWithLookups(ctx context.Context, provider llm.Provider, s setup, req llm.Request, resultChars int) (string, error) {
	tu, ok := provider.(llm.ToolUser)
	if !ok || len(s.kbIDs) == 0 {
		return llm.Complete(ctx, provider, req)
	}
	for lookups := 0; ; {
		req.Tools = lore.Tools
		req.NoMoreTools = lookups >= maxPromptLookups
		t, err := tu.Turn(ctx, req, nil)
		if errors.Is(err, llm.ErrNoTools) {
			req.Tools, req.NoMoreTools = nil, false
			return llm.Complete(ctx, provider, req)
		}
		if err != nil || len(t.Calls) == 0 || req.NoMoreTools {
			return t.Text, err
		}
		msg := llm.Message{Role: llm.RoleAssistant, Calls: t.Calls}
		if strings.TrimSpace(t.Text) != "" {
			msg.Parts = []llm.Part{{Text: t.Text}}
		}
		req.Messages = append(req.Messages, msg)
		for _, call := range t.Calls {
			result := "No lookups left. Answer with the JSON now."
			if lookups < maxPromptLookups {
				lookups++
				result = e.lore.Run(ctx, s.kbIDs, call, resultChars)
			}
			req.Messages = append(req.Messages, llm.Message{Role: llm.RoleTool, CallID: call.ID, Parts: []llm.Part{{Text: result}}})
		}
	}
}

// pictureList tells the model which characters of the scene have a picture
// the drawing can follow, so it names them as the app knows them.
func (e *Engine) pictureList(ctx context.Context, s setup, text string) string {
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		k := strings.ToLower(strings.TrimSpace(n))
		if k != "" && !seen[k] {
			seen[k] = true
			names = append(names, strings.TrimSpace(n))
		}
	}
	if s.card != nil {
		add(s.card.Name)
	}
	// In scenes the narrator tags who speaks: [Name].
	for _, m := range speakerTag.FindAllStringSubmatch(text, -1) {
		if n := strings.TrimSpace(m[1]); !strings.EqualFold(n, Narrator) && !strings.EqualFold(n, "Timeline") {
			add(n)
		}
	}
	add(s.chat.PersonaName)
	var ents []lore.Entity
	if len(s.kbIDs) > 0 {
		ents = e.lore.CachedEntities(ctx, s.kbIDs)
		for _, ent := range lore.Mentioned(ents, strings.ToLower(text), nil) {
			if ent.IsCharacter() {
				add(ent.Name)
			}
		}
	}
	if s.scen != nil {
		for _, m := range s.scen.Cast {
			if lore.MentionAt(strings.ToLower(text), m.Name) >= 0 {
				add(m.Name)
			}
		}
	}
	cards, _ := e.st.Characters()
	var lines []string
	for _, n := range names {
		if len(lines) >= 8 {
			break
		}
		found := ""
		for _, c := range cards {
			if c.Avatar != "" && strings.EqualFold(strings.TrimSpace(c.Name), n) {
				found = "character card picture"
				break
			}
		}
		if found == "" && len(s.kbIDs) > 0 {
			if pic, ok := e.lore.CharacterPicture(ctx, s.kbIDs, n); ok {
				found = "drawn in the source, " + pic.Label
			}
		}
		if found != "" {
			lines = append(lines, "- "+n+" ("+found+")")
		}
	}
	if len(lines) == 0 {
		return "(none: describe everyone's looks fully in the prompt)"
	}
	return strings.Join(lines, "\n")
}

// deleteImages removes a deleted chat's pictures, unless a branch of it
// still shows them.
func (e *Engine) deleteImages(chatID string) {
	if e.img.Dir == "" || e.st.MessagesContaining("[Image: "+chatID+"/", chatID) > 0 {
		return
	}
	if p, err := e.ImagePath(chatID); err == nil && chatID != "" {
		os.RemoveAll(p)
	}
}

// fitLast keeps the newest messages that fit in maxChars.
func fitLast(msgs []store.Message, maxChars int) []store.Message {
	total := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		total += len([]rune(msgs[i].Content)) + 30
		if total > maxChars && i < len(msgs)-1 {
			return msgs[i+1:]
		}
	}
	return msgs
}

// jsonObject is the first {...} object in a model's answer, or "".
func jsonObject(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return ""
	}
	obj := s[start : end+1]
	if !json.Valid([]byte(obj)) {
		return ""
	}
	return obj
}

// imageSystem is the system prompt's note on drawing, when the model may draw.
func (e *Engine) imageSystem() string {
	if !e.imagesOn() {
		return ""
	}
	return "\n\n" + prompts.Get("chat-images")
}
