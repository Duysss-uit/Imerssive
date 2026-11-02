// Package rag is the typed Go facade over the worker's vector store.
//
// Every stored document carries metadata: kb_id (knowledge base or "chat:<id>"
// for conversation memory), source_id, level and optional page range.
// Levels: overview > section (summaries) > entity (character/place profiles) > raw (original text) > memory (chat).
//
// Text vectors live in an embedding Space. The zero Space is the worker's local
// fastembed model; a knowledge base can instead use a provider's embedding API,
// in which case Go computes the vectors and the worker only stores them.
package rag

import (
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"immersive/internal/worker"
)

const (
	LevelOverview = "overview"
	LevelSection  = "section"
	LevelEntity   = "entity"
	LevelRaw      = "raw"
	LevelMemory   = "memory"
)

type Doc struct {
	ID       string         `json:"id"`
	Text     string         `json:"text"`
	Metadata map[string]any `json:"metadata"`
}

type ImageDoc struct {
	ID        string         `json:"id"`
	ImagePath string         `json:"image_path"`
	Metadata  map[string]any `json:"metadata"`
}

type Hit struct {
	ID       string         `json:"id"`
	Text     string         `json:"text"`
	Metadata map[string]any `json:"metadata"`
	Distance float64        `json:"distance"`
}

func (h Hit) Str(key string) string {
	v, _ := h.Metadata[key].(string)
	return v
}

// EmbedFunc computes vectors for texts (query = embedding a search query).
type EmbedFunc func(ctx context.Context, texts []string, query bool) ([][]float32, error)

// Space is the embedding space a set of documents lives in. Each space has
// its own collection, so vectors of different models never mix.
type Space struct {
	Name  string    // collection key; empty = the worker's local text model
	Embed EmbedFunc // nil = the worker embeds locally
}

// APISpace names the space of an API embedding model. The hash keeps names
// unique after the worker truncates long collection names.
func APISpace(providerID, model string, embed EmbedFunc) Space {
	sum := sha1.Sum([]byte(providerID + "|" + model))
	return Space{Name: "api-" + hex.EncodeToString(sum[:])[:10] + "-" + model, Embed: embed}
}

// Spaces returns the embedding space of a knowledge base id. Unknown ids
// (e.g. chat memory) get the local space.
type Spaces func(kbID string) (Space, error)

type Store struct{ w *worker.Manager }

func New(w *worker.Manager) *Store { return &Store{w: w} }

func (s *Store) AddTexts(ctx context.Context, sp Space, docs []Doc) error {
	const batch = 64
	for i := 0; i < len(docs); i += batch {
		chunk := docs[i:min(i+batch, len(docs))]
		params := map[string]any{"items": chunk, "space": sp.Name}
		if sp.Embed != nil {
			texts := make([]string, len(chunk))
			for j, d := range chunk {
				texts[j] = d.Text
			}
			vecs, err := sp.Embed(ctx, texts, false)
			if err != nil {
				return err
			}
			params["embeddings"] = vecs
		}
		if err := s.w.Call(ctx, "store.add_texts", params, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) AddImages(ctx context.Context, docs []ImageDoc) error {
	const batch = 16
	for i := 0; i < len(docs); i += batch {
		end := min(i+batch, len(docs))
		if err := s.w.Call(ctx, "store.add_images", map[string]any{"items": docs[i:end]}, nil); err != nil {
			return err
		}
	}
	return nil
}

// Filter narrows a search. Empty fields are ignored.
type Filter struct {
	KBIDs  []string
	Levels []string
	Source string
}

func (f Filter) where() map[string]any {
	var conds []map[string]any
	if len(f.KBIDs) > 0 {
		conds = append(conds, map[string]any{"kb_id": map[string]any{"$in": f.KBIDs}})
	}
	if len(f.Levels) > 0 {
		conds = append(conds, map[string]any{"level": map[string]any{"$in": f.Levels}})
	}
	if f.Source != "" {
		conds = append(conds, map[string]any{"source_id": f.Source})
	}
	switch len(conds) {
	case 0:
		return nil
	case 1:
		return conds[0]
	}
	return map[string]any{"$and": conds}
}

func (s *Store) SearchTexts(ctx context.Context, sp Space, query string, f Filter, k int) ([]Hit, error) {
	params := map[string]any{"query": query, "k": k, "where": f.where(), "space": sp.Name}
	if sp.Embed != nil {
		vecs, err := sp.Embed(ctx, []string{query}, true)
		if err != nil {
			return nil, err
		}
		params["embedding"] = vecs[0]
	}
	var res struct {
		Hits []Hit `json:"hits"`
	}
	err := s.w.Call(ctx, "store.query_texts", params, &res)
	return res.Hits, err
}

// SearchKBs searches knowledge bases that may live in different embedding
// spaces: one query per space, merged by distance.
func (s *Store) SearchKBs(ctx context.Context, spaces Spaces, query string, f Filter, k int) ([]Hit, error) {
	groups := map[string][]string{}
	bySpace := map[string]Space{}
	var order []string
	var firstErr error
	for _, id := range f.KBIDs {
		sp, err := spaces(id)
		if err != nil {
			// One broken space (e.g. a deleted embedding provider) shouldn't blind the rest.
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		if _, ok := bySpace[sp.Name]; !ok {
			bySpace[sp.Name] = sp
			order = append(order, sp.Name)
		}
		groups[sp.Name] = append(groups[sp.Name], id)
	}
	var all []Hit
	for _, name := range order {
		g := f
		g.KBIDs = groups[name]
		hits, err := s.SearchTexts(ctx, bySpace[name], query, g, k)
		if err != nil {
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		all = append(all, hits...)
	}
	if len(all) == 0 && firstErr != nil {
		return nil, firstErr
	}
	if len(order) > 1 {
		sort.SliceStable(all, func(i, j int) bool { return all[i].Distance < all[j].Distance })
		all = all[:min(k, len(all))]
	}
	return all, nil
}

// SearchImages returns comic pages whose image embedding matches the text query.
// Hit.Text holds the page image path.
func (s *Store) SearchImages(ctx context.Context, query string, f Filter, k int) ([]Hit, error) {
	var res struct {
		Hits []Hit `json:"hits"`
	}
	err := s.w.Call(ctx, "store.query_images", map[string]any{"query": query, "k": k, "where": f.where()}, &res)
	return res.Hits, err
}

// GetTexts fetches documents by filter without similarity ranking.
func (s *Store) GetTexts(ctx context.Context, sp Space, f Filter, limit int) ([]Hit, error) {
	var res struct {
		Hits []Hit `json:"hits"`
	}
	err := s.w.Call(ctx, "store.get_texts", map[string]any{"where": f.where(), "limit": limit, "space": sp.Name}, &res)
	return res.Hits, err
}

// Delete removes matching documents from every collection.
func (s *Store) Delete(ctx context.Context, f Filter) error {
	return s.w.Call(ctx, "store.delete", map[string]any{"where": f.where()}, nil)
}

// Move re-embeds a knowledge base's text documents into another space and
// removes them from the old one. No LLM calls: the stored texts are reused.
func (s *Store) Move(ctx context.Context, kbID string, from, to Space) (int, error) {
	if from.Name == to.Name {
		return 0, nil
	}
	f := Filter{KBIDs: []string{kbID}}
	hits, err := s.GetTexts(ctx, from, f, 1_000_000)
	if err != nil {
		return 0, err
	}
	docs := make([]Doc, len(hits))
	for i, h := range hits {
		docs[i] = Doc{ID: h.ID, Text: h.Text, Metadata: h.Metadata}
	}
	if err := s.AddTexts(ctx, to, docs); err != nil {
		return 0, err
	}
	return len(docs), s.w.Call(ctx, "store.delete", map[string]any{"where": f.where(), "space": from.Name, "only_space": true}, nil)
}

var levelRank = map[string]int{LevelOverview: 0, LevelEntity: 1, LevelSection: 2, LevelMemory: 3, LevelRaw: 4}

// FormatContext renders hits as a compact block for the prompt, grouped so
// higher-level summaries come before raw excerpts.
func FormatContext(hits []Hit, maxChars int) string {
	sorted := append([]Hit(nil), hits...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return levelRank[sorted[i].Str("level")] < levelRank[sorted[j].Str("level")]
	})
	var sb strings.Builder
	seen := map[string]bool{} // the same passage can be stored twice (e.g. a file added twice)
	for _, h := range sorted {
		key := strings.Join(strings.Fields(h.Text), " ")
		if seen[key] {
			continue
		}
		seen[key] = true
		label := map[string]string{
			LevelOverview: "Tổng quan", LevelEntity: "Hồ sơ", LevelSection: "Tóm tắt",
			LevelRaw: "Trích đoạn", LevelMemory: "Ký ức hội thoại",
		}[h.Str("level")]
		if label == "" {
			label = "Ghi chú"
		}
		if name := h.Str("name"); name != "" {
			label += ": " + name
		}
		if p := h.Str("pages"); p != "" {
			label += " (tr. " + p + ")"
		}
		entry := fmt.Sprintf("[%s]\n%s\n\n", label, strings.TrimSpace(h.Text))
		if maxChars > 0 && sb.Len()+len(entry) > maxChars {
			break
		}
		sb.WriteString(entry)
	}
	return strings.TrimSpace(sb.String())
}
