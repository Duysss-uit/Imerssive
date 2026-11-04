package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"immersive/internal/llm"
	"immersive/internal/lore"
	"immersive/internal/prompts"
	"immersive/internal/rag"
	"immersive/internal/store"
)

// Generation doesn't stuff the prompt with retrieved text. The model gets a
// brief (the story map + the names the work knows) and looks up what it needs
// with the lore tools. Models without tool calling first list their queries,
// then get the results in one block.
const (
	briefNames  = 80 // entity names listed in the brief
	maxSearches = 5  // lookups per generation
)

// Entity is one profile the ingest pipeline built (character, place, faction...).
type Entity = lore.Entity

// Entities lists the profiles stored for the knowledge bases.
func (s *Service) Entities(ctx context.Context, kbIDs []string) ([]Entity, error) {
	return s.lore.Entities(ctx, kbIDs)
}

// brief outlines the work in about maxChars: the story map and the names of
// everything profiled.
func (s *Service) brief(ctx context.Context, kbIDs []string, maxChars int) string {
	var names string
	if ents, err := s.Entities(ctx, kbIDs); err == nil && len(ents) > 0 {
		var list []string
		for _, e := range ents[:min(len(ents), briefNames)] {
			list = append(list, fmt.Sprintf("%s (%s)", e.Name, e.Type))
		}
		names = "[Có hồ sơ trong truyện]\n" + strings.Join(list, ", ")
	}
	m := s.lore.Map(kbIDs, max(maxChars-len([]rune(names)), maxChars/2))
	if m != "" {
		m = "[Bản đồ truyện]\n" + m
	}
	return strings.TrimSpace(m + "\n\n" + names)
}

// research runs task with the model looking up what it needs, and returns its final answer.
// extra is added to the brief (e.g. the profile of the character being carded).
func (s *Service) research(ctx context.Context, kbIDs []string, providerID, purpose, task, extra string) (string, error) {
	p, err := s.resolve(llm.RoleSummarize, providerID)
	if err != nil {
		return "", err
	}
	maxOut, inChars := llm.Budget(p, 10000)
	// The brief takes a quarter of the room; each lookup an equal share of the rest.
	brief := strings.TrimSpace(s.brief(ctx, kbIDs, inChars/4) + "\n\n" + headRunes(extra, inChars/8))
	resultChars := max((inChars-len([]rune(brief))-len([]rune(task))-2000)/(maxSearches+1), 800)
	msgs := []llm.Message{llm.TextMessage(llm.RoleUser, task+"\n\n"+prompts.Get("research-tools", "brief", brief, "max", strconv.Itoa(maxSearches)))}
	searches := 0
	for turnNo := 1; ; turnNo++ {
		last := searches >= maxSearches
		turn, err := llm.RunTurn(ctx, p, llm.Request{Purpose: fmt.Sprintf("%s · lượt %d", purpose, turnNo),
			System: prompts.Get("analyst-system"), MaxTokens: maxOut, Messages: msgs, Tools: lore.Tools, NoMoreTools: last})
		if errors.Is(err, llm.ErrNoTools) {
			log.Printf("research: %s: model không gọi được công cụ, chuyển sang tìm theo kế hoạch", purpose)
			return s.researchPlanned(ctx, p, maxOut, inChars, kbIDs, purpose, task, brief)
		}
		if err != nil {
			return "", err
		}
		if len(turn.Calls) == 0 || last {
			return turn.Text, nil
		}
		reply := llm.Message{Role: llm.RoleAssistant, Calls: turn.Calls}
		if strings.TrimSpace(turn.Text) != "" {
			reply.Parts = []llm.Part{{Text: turn.Text}}
		}
		msgs = append(msgs, reply)
		for _, c := range turn.Calls {
			result := "Đã hết lượt tra cứu. Hãy viết kết quả từ những gì đã có."
			if searches < maxSearches {
				searches++
				result = s.lore.Run(ctx, kbIDs, c, resultChars)
				log.Printf("research: %s %s → %d ký tự", c.Name, c.Args, len([]rune(result)))
			}
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, CallID: c.ID, Parts: []llm.Part{{Text: result}}})
		}
	}
}

// researchPlanned is research for models without tool calling: they list
// their queries first, then answer with all the results at once.
func (s *Service) researchPlanned(ctx context.Context, p llm.Provider, maxOut, inChars int, kbIDs []string, purpose, task, brief string) (string, error) {
	plan, err := llm.Complete(ctx, p, llm.Request{Purpose: purpose + " · lập danh sách cần tìm", System: prompts.Get("analyst-system"),
		MaxTokens: min(maxOut, 4000), Messages: []llm.Message{llm.TextMessage(llm.RoleUser, prompts.Get("research-plan", "task", task, "brief", brief, "max", strconv.Itoa(maxSearches)))}})
	if err != nil {
		return "", err
	}
	var queries []string
	if i, j := strings.Index(plan, "["), strings.LastIndex(plan, "]"); i >= 0 && j > i {
		json.Unmarshal([]byte(plan[i:j+1]), &queries)
	}
	if len(queries) == 0 {
		queries = []string{task}
	}
	var hits []rag.Hit
	for _, q := range queries[:min(len(queries), maxSearches)] {
		h, err := s.lore.Search(ctx, kbIDs, q, 4)
		log.Printf("research: tìm %q → %d kết quả", q, len(h))
		if err != nil {
			return "", err
		}
		hits = append(hits, h...)
	}
	knowledge := rag.FormatContext(hits, max(inChars-len([]rune(brief))-len([]rune(task))-2000, 1500))
	return llm.Complete(ctx, p, llm.Request{Purpose: purpose, System: prompts.Get("analyst-system"), MaxTokens: maxOut,
		Messages: []llm.Message{llm.TextMessage(llm.RoleUser, task+"\n\n"+prompts.Get("research-knowledge", "brief", brief, "knowledge", knowledge))}})
}

func hintLine(hint string) string {
	if strings.TrimSpace(hint) == "" {
		return ""
	}
	return "Extra direction from the user: " + hint
}

// GenerateCharacter drafts a character card from knowledge bases with the given
// provider (empty = the summarize role). The result is returned unsaved so the
// user can edit it first.
func (s *Service) GenerateCharacter(ctx context.Context, kbIDs []string, name, hint, providerID string) (store.Character, error) {
	// Their own profile is the obvious starting point; hand it over up front.
	var extra string
	if ents, err := s.Entities(ctx, kbIDs); err == nil {
		for _, e := range ents {
			if strings.EqualFold(strings.TrimSpace(e.Name), strings.TrimSpace(name)) {
				extra += fmt.Sprintf("[Hồ sơ: %s]\n%s\n", e.Name, e.Profile)
			}
		}
	}
	out, err := s.research(ctx, kbIDs, providerID, "tạo nhân vật "+name, prompts.Get("character-card", "name", name, "hint", hintLine(hint)), extra)
	if err != nil {
		return store.Character{}, err
	}
	var c store.Character
	if err := ParseJSON(out, &c); err != nil {
		return c, fmt.Errorf("model trả về sai định dạng: %w", err)
	}
	if c.Name == "" {
		c.Name = name
	}
	c.KBIDs = kbIDs
	return c, nil
}

func (s *Service) GenerateScenario(ctx context.Context, kbIDs []string, hint, providerID string) (store.Scenario, error) {
	out, err := s.research(ctx, kbIDs, providerID, "tạo bối cảnh", prompts.Get("scenario", "hint", hintLine(hint)), "")
	if err != nil {
		return store.Scenario{}, err
	}
	var sc store.Scenario
	if err := ParseJSON(out, &sc); err != nil {
		return sc, fmt.Errorf("model trả về sai định dạng: %w", err)
	}
	sc.KBIDs = kbIDs
	return sc, nil
}

// ExtractCast lists the source characters who fit a scenario the user wrote
// (GenerateScenario already returns a cast for generated ones).
func (s *Service) ExtractCast(ctx context.Context, kbIDs []string, title, setting, providerID string) ([]store.CastMember, error) {
	out, err := s.research(ctx, kbIDs, providerID, "trích xuất nhân vật "+title, prompts.Get("cast", "title", title, "setting", setting), "")
	if err != nil {
		return nil, err
	}
	var res struct {
		Cast []store.CastMember `json:"cast"`
	}
	if err := ParseJSON(out, &res); err != nil {
		return nil, fmt.Errorf("model trả về sai định dạng: %w", err)
	}
	return res.Cast, nil
}

// headRunes keeps the first n runes of s.
func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Shorten keeps about n runes of s, cut at a sentence or word end when possible.
func Shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	cut := string(r[:n])
	if i := strings.LastIndexAny(cut, ".!?。"); i > n/2 {
		return cut[:i+1]
	}
	if i := strings.LastIndex(cut, " "); i > n/2 {
		return cut[:i] + "…"
	}
	return cut + "…"
}
