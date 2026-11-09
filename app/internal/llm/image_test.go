package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImagerChatSendsReferences(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("request %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"choices":[{"message":{"content":"","images":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}}]}`)
	}))
	defer srv.Close()
	im, err := NewImager(Config{Kind: KindOpenRouter, BaseURL: srv.URL, Model: "google/gemini-image", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	img, err := im.Draw(context.Background(), ImageRequest{Prompt: "Mai Linh in the rain", Aspect: "portrait",
		Refs: []RefImage{{Label: "Mai Linh: how she looks", B64: "BBBB", MIME: "image/jpeg"}}})
	if err != nil || img.B64 != "AAAA" || img.MIME != "image/png" {
		t.Fatalf("img=%+v err=%v", img, err)
	}
	b, _ := json.Marshal(body)
	s := string(b)
	for _, want := range []string{`"modalities":["image","text"]`, `"aspect_ratio":"2:3"`, `Reference 1: Mai Linh: how she looks`, `data:image/jpeg;base64,BBBB`} {
		if !strings.Contains(s, want) {
			t.Errorf("request lacks %s:\n%s", want, s)
		}
	}
}

func TestImagerChatTextOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"content":"I can't draw."}}]}`)
	}))
	defer srv.Close()
	im, _ := NewImager(Config{Kind: KindOpenRouter, BaseURL: srv.URL, Model: "x"})
	if _, err := im.Draw(context.Background(), ImageRequest{Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "I can't draw") {
		t.Errorf("err = %v", err)
	}
}

func TestImagerImagesEditsWithReferences(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/edits" {
			t.Errorf("path %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if got := r.MultipartForm.File["image[]"]; len(got) != 2 {
			t.Errorf("references sent: %d", len(got))
		}
		if p := r.FormValue("prompt"); !strings.Contains(p, "1. Lâm Phong") || r.FormValue("size") != "1536x1024" {
			t.Errorf("prompt %q size %q", p, r.FormValue("size"))
		}
		io.WriteString(w, `{"data":[{"b64_json":"CCCC"}]}`)
	}))
	defer srv.Close()
	im, _ := NewImager(Config{Kind: KindOpenAI, BaseURL: srv.URL, Model: "gpt-image-1"})
	img, err := im.Draw(context.Background(), ImageRequest{Prompt: "a duel", Aspect: "landscape",
		Refs: []RefImage{{Label: "Lâm Phong", B64: "AAAA", MIME: "image/png"}, {Label: "Hắc Sơn", B64: "AAAA", MIME: "image/jpeg"}}})
	if err != nil || img.B64 != "CCCC" {
		t.Fatalf("img=%+v err=%v", img, err)
	}
}

func TestImagerSDWebUI(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		io.WriteString(w, `{"images":["DDDD"]}`)
	}))
	defer srv.Close()
	im, _ := NewImager(Config{Kind: KindLocal, BaseURL: srv.URL})
	ref := []RefImage{{Label: "previous", B64: "AAAA", MIME: "image/png"}}
	if img, err := im.Draw(context.Background(), ImageRequest{Prompt: "p", Refs: ref}); err != nil || img.B64 != "DDDD" || path != "/sdapi/v1/txt2img" {
		t.Errorf("txt2img: %+v %v %s", img, err, path)
	}
	im.Draw(context.Background(), ImageRequest{Prompt: "p", Refs: ref, Continue: true})
	if path != "/sdapi/v1/img2img" {
		t.Errorf("continuing should draw over the previous picture: %s", path)
	}
}

func TestImagerFallsBackWithoutEdits(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/images/edits" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"data":[{"b64_json":"EEEE"}]}`)
	}))
	defer srv.Close()
	im, _ := NewImager(Config{Kind: KindOpenAI, BaseURL: srv.URL, Model: "grok-imagine-image", ImageAPI: ImageAPIImages})
	img, err := im.Draw(context.Background(), ImageRequest{Prompt: "p", Refs: []RefImage{{Label: "x", B64: "AAAA", MIME: "image/png"}}})
	if err != nil || img.B64 != "EEEE" || strings.Join(paths, ",") != "/images/edits,/images/generations" {
		t.Errorf("img=%+v err=%v paths=%v", img, err, paths)
	}
}
