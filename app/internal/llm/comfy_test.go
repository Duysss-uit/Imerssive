package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const comfyWithRefs = `{
 "1": {"class_type": "LoadImage", "inputs": {"image": "{ref1}"}},
 "2": {"class_type": "LoadImage", "inputs": {"image": "{ref2}"}},
 "3": {"class_type": "CLIPTextEncode", "inputs": {"text": "{prompt}. Refs: {ref_labels}"}},
 "4": {"class_type": "EmptyLatentImage", "inputs": {"width": "{width}", "height": "{height}", "batch_size": 1}},
 "5": {"class_type": "KSampler", "inputs": {"seed": "{seed}"}},
 "6": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "{model}"}},
 "9": {"class_type": "SaveImage", "inputs": {"images": ["8", 0]}}
}`

const comfyNoRefs = `{
 "3": {"class_type": "CLIPTextEncode", "inputs": {"text": "{prompt}"}},
 "9": {"class_type": "SaveImage", "inputs": {}}
}`

// fakeComfy answers like ComfyUI: uploads, a queued job that finishes on the
// second poll, and the saved picture.
func fakeComfy(t *testing.T) (*httptest.Server, *[]map[string]any, *[]string) {
	var mu sync.Mutex
	var graphs []map[string]any
	var uploads []string
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/upload/image":
			r.ParseMultipartForm(1 << 20)
			f := r.MultipartForm.File["image"][0]
			uploads = append(uploads, f.Filename)
			json.NewEncoder(w).Encode(map[string]string{"name": f.Filename, "subfolder": "", "type": "input"})
		case r.URL.Path == "/prompt":
			var body struct {
				Prompt map[string]any `json:"prompt"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			graphs = append(graphs, body.Prompt)
			io.WriteString(w, `{"prompt_id": "job1", "number": 1, "node_errors": {}}`)
		case r.URL.Path == "/history/job1":
			polls++
			if polls == 1 {
				io.WriteString(w, `{}`)
				return
			}
			io.WriteString(w, `{"job1": {"outputs": {"9": {"images": [{"filename": "immersive_0001.png", "subfolder": "", "type": "output"}]}},
				"status": {"status_str": "success", "completed": true, "messages": []}}}`)
		case r.URL.Path == "/view":
			if r.URL.Query().Get("filename") != "immersive_0001.png" || r.URL.Query().Get("type") != "output" {
				t.Errorf("view %s", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("PNGDATA"))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	return srv, &graphs, &uploads
}

func TestComfyFillsWorkflow(t *testing.T) {
	srv, graphs, uploads := fakeComfy(t)
	defer srv.Close()
	im, err := NewImager(Config{Kind: KindLocal, BaseURL: srv.URL, ImageAPI: ImageAPIComfyUI, Model: "sdxl.safetensors",
		Workflow: comfyWithRefs, WorkflowNoRef: comfyNoRefs})
	if err != nil {
		t.Fatal(err)
	}
	// Three references, two slots: the first two are uploaded and used.
	refs := []RefImage{{Label: "Mai Linh", B64: "QQ==", MIME: "image/png"}, {Label: "Lâm Phong", B64: "QQ==", MIME: "image/jpeg"}, {Label: "page", B64: "QQ==", MIME: "image/jpeg"}}
	img, err := im.Draw(context.Background(), ImageRequest{Prompt: "a duel in the rain", Aspect: "portrait", Refs: refs})
	if err != nil || img.B64 != "UE5HREFUQQ==" || img.MIME != "image/png" {
		t.Fatalf("img=%+v err=%v", img, err)
	}
	if len(*uploads) != 2 {
		t.Fatalf("uploads: %v", *uploads)
	}
	g := (*graphs)[0]
	in := func(id string) map[string]any { return g[id].(map[string]any)["inputs"].(map[string]any) }
	if in("1")["image"] != (*uploads)[0] || in("2")["image"] != (*uploads)[1] {
		t.Errorf("reference slots: %v %v", in("1"), in("2"))
	}
	if in("4")["width"] != float64(832) || in("4")["height"] != float64(1216) {
		t.Errorf("size should be numbers: %#v", in("4"))
	}
	if _, ok := in("5")["seed"].(float64); !ok {
		t.Errorf("seed: %#v", in("5")["seed"])
	}
	if text := in("3")["text"].(string); !strings.HasPrefix(text, "a duel in the rain. Refs: image 1: Mai Linh; image 2: Lâm Phong") {
		t.Errorf("prompt: %q", text)
	}
	if in("6")["ckpt_name"] != "sdxl.safetensors" {
		t.Errorf("model: %v", in("6"))
	}

	// No references: the workflow without slots runs, nothing is uploaded.
	if _, err := im.Draw(context.Background(), ImageRequest{Prompt: "a quiet temple"}); err != nil {
		t.Fatal(err)
	}
	if len(*uploads) != 2 || len(*graphs) != 2 {
		t.Fatalf("uploads %d graphs %d", len(*uploads), len(*graphs))
	}
	if _, ok := (*graphs)[1]["1"]; ok {
		t.Error("the workflow with reference slots ran without references")
	}
}

func TestComfyWorkflowChecks(t *testing.T) {
	if err := CheckComfyWorkflow(`{"nodes": [], "links": []}`); err == nil {
		t.Error("a UI-format workflow should be refused")
	}
	if err := CheckComfyWorkflow(`{"3": {"class_type": "KSampler", "inputs": {}}}`); err == nil || !strings.Contains(err.Error(), "{prompt}") {
		t.Errorf("missing {prompt}: %v", err)
	}
	if ComfyRefSlots(comfyWithRefs) != 2 || ComfyRefSlots(comfyNoRefs) != 0 {
		t.Error("slot count")
	}
	// Only a workflow with slots and no references: say what to add.
	im, _ := NewImager(Config{Kind: KindLocal, BaseURL: "http://127.0.0.1:1", ImageAPI: ImageAPIComfyUI, Workflow: comfyWithRefs})
	if _, err := im.Draw(context.Background(), ImageRequest{Prompt: "p"}); err == nil || !strings.Contains(err.Error(), "không tham chiếu") {
		t.Errorf("err = %v", err)
	}
}

func TestImageModelsComfy(t *testing.T) {
	for _, body := range []string{
		`{"CheckpointLoaderSimple": {"input": {"required": {"ckpt_name": [["a.safetensors", "b.safetensors"], {}]}}}}`,
		`{"CheckpointLoaderSimple": {"input": {"required": {"ckpt_name": ["COMBO", {"options": ["a.safetensors", "b.safetensors"]}]}}}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/object_info/CheckpointLoaderSimple" {
				t.Errorf("path %s", r.URL.Path)
			}
			io.WriteString(w, body)
		}))
		got, err := ImageModels(context.Background(), Config{Kind: KindLocal, BaseURL: srv.URL, ImageAPI: ImageAPIComfyUI})
		srv.Close()
		if err != nil || strings.Join(got, ",") != "a.safetensors,b.safetensors" {
			t.Errorf("got %v err %v", got, err)
		}
	}
}
