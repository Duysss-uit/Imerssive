package ingest

import (
	"context"
	"fmt"
	"strings"

	"immersive/internal/llm"
	"immersive/internal/rag"
	"immersive/internal/store"
)

// gatherKnowledge pulls what the KBs know about a topic: KB overviews plus
// the best-matching entity profiles and section summaries.
// Knowledge budget for generation prompts. Kept well under 8k tokens with the
// instructions, so models loaded with a small context (LM Studio, Ollama)
// still have room to think and answer.
const (
	overviewChars  = 5000
	knowledgeChars = 12000
)

func (s *Service) gatherKnowledge(ctx context.Context, kbIDs []string, query string) (string, error) {
	var parts []string
	for _, id := range kbIDs {
		if kb, err := s.st.KnowledgeBase(id); err == nil && kb.Overview != "" {
			parts = append(parts, "[Tổng quan: "+kb.Name+"]\n"+headRunes(kb.Overview, overviewChars))
		}
	}
	hits, err := s.rag.SearchKBs(ctx, s.spaces, query, rag.Filter{KBIDs: kbIDs,
		Levels: []string{rag.LevelEntity, rag.LevelSection, rag.LevelRaw}}, 10)
	if err != nil {
		return "", err
	}
	parts = append(parts, rag.FormatContext(hits, knowledgeChars))
	return strings.Join(parts, "\n\n"), nil
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
	knowledge, err := s.gatherKnowledge(ctx, kbIDs, name+" "+hint)
	if err != nil {
		return store.Character{}, err
	}
	p, err := s.resolve(llm.RoleSummarize, providerID)
	if err != nil {
		return store.Character{}, err
	}
	out, err := llm.Complete(ctx, p, llm.Request{Purpose: "tạo nhân vật " + name, System: analystSystem, MaxTokens: 10000,
		Messages: []llm.Message{llm.TextMessage(llm.RoleUser, fmt.Sprintf(characterPrompt, name, hintLine(hint), knowledge))}})
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
	q := hint
	if q == "" {
		q = "bối cảnh thế giới, địa điểm, sự kiện chính"
	}
	knowledge, err := s.gatherKnowledge(ctx, kbIDs, q)
	if err != nil {
		return store.Scenario{}, err
	}
	p, err := s.resolve(llm.RoleSummarize, providerID)
	if err != nil {
		return store.Scenario{}, err
	}
	out, err := llm.Complete(ctx, p, llm.Request{Purpose: "tạo bối cảnh", System: analystSystem, MaxTokens: 10000,
		Messages: []llm.Message{llm.TextMessage(llm.RoleUser, fmt.Sprintf(scenarioPrompt, hintLine(hint), knowledge))}})
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
	knowledge, err := s.gatherKnowledge(ctx, kbIDs, title+"\n"+setting)
	if err != nil {
		return nil, err
	}
	p, err := s.resolve(llm.RoleSummarize, providerID)
	if err != nil {
		return nil, err
	}
	out, err := llm.Complete(ctx, p, llm.Request{Purpose: "trích xuất nhân vật " + title, System: analystSystem, MaxTokens: 10000,
		Messages: []llm.Message{llm.TextMessage(llm.RoleUser, fmt.Sprintf(castPrompt, title, setting, knowledge))}})
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
