package chat

import (
	"context"
	"fmt"
	"log"
	"strings"

	"immersive/internal/llm"
	"immersive/internal/prompts"
	"immersive/internal/store"
)

// The divergence log records how a story has departed from its source because
// of what the player did: who died or changed sides, what was learned, where
// everyone is now. It sits in the system prompt and wins over the source, so
// the model builds on the player's story instead of drifting back to canon.
// Every few replies the summarize model folds the new messages into it.

// updateDivergence folds the messages since the last update into the chat's
// divergence log, once enough replies have passed. One update runs per chat
// at a time.
func (e *Engine) updateDivergence(chatID string) {
	e.mu.Lock()
	if e.diverging[chatID] {
		e.mu.Unlock()
		return
	}
	e.diverging[chatID] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.diverging, chatID)
		e.mu.Unlock()
	}()
	if err := e.divergence(context.Background(), chatID, false); err != nil {
		log.Printf("chat: %s cập nhật sổ thay đổi: %v", chatID, err)
	}
}

// divergence updates the log; force runs it whatever the number of new replies.
func (e *Engine) divergence(ctx context.Context, chatID string, force bool) error {
	every := e.opts().DivergenceEvery
	if every <= 0 && !force {
		return nil
	}
	s, err := e.setup(chatID)
	if err != nil {
		return err
	}
	all, err := e.st.Messages(chatID, false)
	if err != nil {
		return err
	}
	var fresh []store.Message
	replies := 0
	for _, m := range all {
		if m.ID > s.chat.DivergenceUpTo {
			fresh = append(fresh, m)
			if m.Role == "assistant" {
				replies++
			}
		}
	}
	if replies == 0 || (!force && replies < every) {
		return nil
	}
	p, err := e.resolve(llm.RoleSummarize, "")
	if err != nil {
		return err
	}
	maxOut, inChars := llm.Budget(p, 3000)
	canon := e.lore.Map(s.kbIDs, inChars/5)
	before := strings.TrimSpace(s.chat.Divergence)
	fresh = fitMessages(fresh, inChars-len([]rune(canon))-len([]rune(before))-2500) // the rest comes next time
	logText := before
	if logText == "" {
		logText = "(empty: nothing differs from the source yet)"
	}
	if canon == "" {
		canon = "(no story map)"
	}
	out, err := llm.Complete(ctx, p, llm.Request{Purpose: "sổ thay đổi · " + s.chat.Title, MaxTokens: maxOut,
		System: prompts.Get("analyst-system"),
		Messages: []llm.Message{llm.TextMessage(llm.RoleUser, prompts.Get("chat-divergence",
			"other", s.other, "player", s.chat.PersonaName, "canon", canon, "log", logText, "excerpt", excerpt(fresh, s)))}})
	if err != nil {
		return err
	}
	out = strings.TrimSpace(strings.Trim(strings.TrimSpace(out), "`"))
	out = strings.TrimSpace(strings.TrimPrefix(out, "markdown"))
	if out == "" {
		return fmt.Errorf("model trả về sổ thay đổi trống")
	}
	// The player edited the log meanwhile: keep their version, fold these
	// messages in next time.
	if now, err := e.st.Chat(chatID); err != nil || strings.TrimSpace(now.Divergence) != before {
		return err
	}
	if err := e.st.SetDivergence(chatID, out, fresh[len(fresh)-1].ID); err != nil {
		return err
	}
	log.Printf("chat: %s cập nhật sổ thay đổi (%d tin nhắn mới)", chatID, len(fresh))
	e.emit("chat:divergence", map[string]any{"chatId": chatID, "divergence": out})
	return nil
}

// RefreshDivergence updates the divergence log now with every message it
// doesn't cover yet.
func (e *Engine) RefreshDivergence(ctx context.Context, chatID string) (string, error) {
	if err := e.divergence(ctx, chatID, true); err != nil {
		return "", err
	}
	c, err := e.st.Chat(chatID)
	return c.Divergence, err
}

// SetDivergence replaces the divergence log with the player's version.
func (e *Engine) SetDivergence(chatID, text string) error {
	return e.st.SetDivergence(chatID, strings.TrimSpace(text), 0)
}

func excerpt(msgs []store.Message, s setup) string {
	var sb strings.Builder
	for _, m := range msgs {
		who := s.chat.PersonaName
		if m.Role == "assistant" {
			who = s.other
		}
		fmt.Fprintf(&sb, "%s: %s\n\n", who, strings.TrimSpace(m.Content))
	}
	return sb.String()
}

// fitMessages keeps the oldest messages that fit in maxChars (always at
// least one, shortened if it alone is too long).
func fitMessages(msgs []store.Message, maxChars int) []store.Message {
	maxChars = max(maxChars, 1000)
	total := 0
	for i, m := range msgs {
		total += len([]rune(m.Content)) + 30
		if total > maxChars {
			if i == 0 {
				m.Content = string([]rune(m.Content)[:maxChars-30])
				return []store.Message{m}
			}
			return msgs[:i]
		}
	}
	return msgs
}
