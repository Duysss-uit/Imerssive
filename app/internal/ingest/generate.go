package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"immersive/internal/llm"
	"immersive/internal/rag"
	"immersive/internal/store"
)

// Generation doesn't stuff the prompt with retrieved text. The model gets a
// brief (overview + the names the work knows) and looks up what it needs with
// the search_story tool. Models without tool calling first list their queries,
// then get the results in one block.
const (
	briefOverviewChars = 3000  // overview per knowledge base in the brief
	briefNames         = 80    // entity names listed in the brief
	searchResultChars  = 5000  // one search_story result
	maxSearches        = 5     // searches per generation
	plannedChars       = 12000 // all results together, without tools
)

// lookup is the part of the vector store generation needs.
type lookup interface {
	SearchKBs(ctx context.Context, spaces rag.Spaces, query string, f rag.Filter, k int) ([]rag.Hit, error)
	GetTexts(ctx context.Context, sp rag.Space, f rag.Filter, limit int) ([]rag.Hit, error)
}

var searchTool = llm.Tool{
	Name: "search_story",
	Description: "Search the work's knowledge base: character profiles, chapter summaries and original passages. " +
		"Use one focused query per call: a name, a place, an event, a relationship.",
	Params:   map[string]any{"query": map[string]any{"type": "string", "description": "what to look up, in the work's language"}},
	Required: []string{"query"},
}

// Entity is one profile the ingest pipeline built (character, place, faction...).
type Entity struct {
	Name, Type, Profile string
}

// Entities lists the profiles stored for the knowledge bases.
func (s *Service) Entities(ctx context.Context, kbIDs []string) ([]Entity, error) {
	var out []Entity
	for _, id := range kbIDs {
		sp, err := s.spaces(id)
		if err != nil {
			return nil, err
		}
		hits, err := s.lookup.GetTexts(ctx, sp, rag.Filter{KBIDs: []string{id}, Levels: []string{rag.LevelEntity}}, 1000)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			out = append(out, parseEntity(h.Str("name"), h.Text))
		}
	}
	return out, nil
}

// parseEntity splits a stored profile "Name (type): profile".
func parseEntity(name, text string) Entity {
	e := Entity{Name: name, Profile: text}
	if head, rest, ok := strings.Cut(text, ": "); ok && strings.HasSuffix(head, ")") {
		if i := strings.LastIndex(head, " ("); i > 0 {
			e.Type = head[i+2 : len(head)-1]
			e.Profile = rest
			if e.Name == "" {
				e.Name = head[:i]
			}
		}
	}
	return e
}

// IsCharacter reports whether the profile is about a person (types come from the model, in any language).
func (e Entity) IsCharacter() bool {
	t := strings.ToLower(e.Type)
	return strings.Contains(t, "character") || strings.Contains(t, "nhân vật") || strings.Contains(t, "person")
}

// brief outlines the work: overviews and the names of everything profiled.
func (s *Service) brief(ctx context.Context, kbIDs []string) string {
	var parts []string
	for _, id := range kbIDs {
		if kb, err := s.st.KnowledgeBase(id); err == nil && kb.Overview != "" {
			parts = append(parts, "[Tổng quan: "+kb.Name+"]\n"+headRunes(kb.Overview, briefOverviewChars))
		}
	}
	if ents, err := s.Entities(ctx, kbIDs); err == nil && len(ents) > 0 {
		var names []string
		for _, e := range ents[:min(len(ents), briefNames)] {
			names = append(names, fmt.Sprintf("%s (%s)", e.Name, e.Type))
		}
		parts = append(parts, "[Có hồ sơ trong truyện]\n"+strings.Join(names, ", "))
	}
	return strings.Join(parts, "\n\n")
}

// search answers one query against the knowledge bases.
func (s *Service) search(ctx context.Context, kbIDs []string, query string, k int) ([]rag.Hit, error) {
	return s.lookup.SearchKBs(ctx, s.spaces, query, rag.Filter{KBIDs: kbIDs,
		Levels: []string{rag.LevelEntity, rag.LevelSection, rag.LevelRaw}}, k)
}

// research runs task with the model looking up what it needs, and returns its final answer.
// extra is added to the brief (e.g. the profile of the character being carded).
func (s *Service) research(ctx context.Context, kbIDs []string, providerID, purpose, task, extra string) (string, error) {
	p, err := s.resolve(llm.RoleSummarize, providerID)
	if err != nil {
		return "", err
	}
	brief := strings.TrimSpace(s.brief(ctx, kbIDs) + "\n\n" + extra)
	msgs := []llm.Message{llm.TextMessage(llm.RoleUser, task+"\n\n"+fmt.Sprintf(researchNote, brief, maxSearches))}
	searches := 0
	for turnNo := 1; ; turnNo++ {
		last := searches >= maxSearches
		turn, err := llm.RunTurn(ctx, p, llm.Request{Purpose: fmt.Sprintf("%s · lượt %d", purpose, turnNo),
			System: analystSystem, MaxTokens: 10000, Messages: msgs, Tools: []llm.Tool{searchTool}, NoMoreTools: last})
		if errors.Is(err, llm.ErrNoTools) {
			log.Printf("research: %s: model không gọi được công cụ, chuyển sang tìm theo kế hoạch", purpose)
			return s.researchPlanned(ctx, p, kbIDs, purpose, task, brief)
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
			result := "Đã hết lượt tìm. Hãy viết kết quả từ những gì đã có."
			if searches < maxSearches {
				searches++
				result = s.runSearch(ctx, kbIDs, c)
			}
			msgs = append(msgs, llm.Message{Role: llm.RoleTool, CallID: c.ID, Parts: []llm.Part{{Text: result}}})
		}
	}
}

// runSearch executes one search_story call and formats the result for the model.
func (s *Service) runSearch(ctx context.Context, kbIDs []string, c llm.ToolCall) string {
	var args struct {
		Query string `json:"query"`
	}
	if c.Name != searchTool.Name || json.Unmarshal([]byte(c.Args), &args) != nil || strings.TrimSpace(args.Query) == "" {
		return `Lỗi: gọi search_story với {"query": "..."}.`
	}
	hits, err := s.search(ctx, kbIDs, args.Query, 6)
	log.Printf("research: tìm %q → %d kết quả", args.Query, len(hits))
	if err != nil {
		return "Lỗi khi tìm: " + err.Error()
	}
	if len(hits) == 0 {
		return "Không tìm thấy gì cho câu hỏi này."
	}
	return rag.FormatContext(hits, searchResultChars)
}

// researchPlanned is research for models without tool calling: they list
// their queries first, then answer with all the results at once.
func (s *Service) researchPlanned(ctx context.Context, p llm.Provider, kbIDs []string, purpose, task, brief string) (string, error) {
	plan, err := llm.Complete(ctx, p, llm.Request{Purpose: purpose + " · lập danh sách cần tìm", System: analystSystem,
		MaxTokens: 4000, Messages: []llm.Message{llm.TextMessage(llm.RoleUser, fmt.Sprintf(planPrompt, task, brief, maxSearches))}})
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
		h, err := s.search(ctx, kbIDs, q, 4)
		log.Printf("research: tìm %q → %d kết quả", q, len(h))
		if err != nil {
			return "", err
		}
		hits = append(hits, h...)
	}
	return llm.Complete(ctx, p, llm.Request{Purpose: purpose, System: analystSystem, MaxTokens: 10000,
		Messages: []llm.Message{llm.TextMessage(llm.RoleUser, task+"\n\n"+fmt.Sprintf(knowledgeNote, brief, rag.FormatContext(hits, plannedChars)))}})
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
	out, err := s.research(ctx, kbIDs, providerID, "tạo nhân vật "+name, fmt.Sprintf(characterPrompt, name, hintLine(hint)), extra)
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
	out, err := s.research(ctx, kbIDs, providerID, "tạo bối cảnh", fmt.Sprintf(scenarioPrompt, hintLine(hint)), "")
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
	out, err := s.research(ctx, kbIDs, providerID, "trích xuất nhân vật "+title, fmt.Sprintf(castPrompt, title, setting), "")
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
