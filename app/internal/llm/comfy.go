package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ImageAPIComfyUI draws by running the player's own ComfyUI workflow
// (exported with "Save (API Format)"), with placeholders filled in:
//
//	{prompt} {negative} {width} {height} {seed} {model} {ref_labels}
//	{ref1} {ref2} ... (the uploaded reference pictures, for LoadImage nodes)
const ImageAPIComfyUI = "comfyui"

// DefaultComfyURL is where a local ComfyUI listens.
const DefaultComfyURL = "http://127.0.0.1:8188"

const comfyNegative = "lowres, bad anatomy, bad hands, extra fingers, text, watermark, speech bubble, blurry, jpeg artifacts"

var refSlot = regexp.MustCompile(`\{ref(\d+)\}`)

// ComfyRefSlots is how many reference pictures a workflow takes ({ref1}..{refN}).
func ComfyRefSlots(workflow string) int {
	n := 0
	for _, m := range refSlot.FindAllStringSubmatch(workflow, -1) {
		if k, _ := strconv.Atoi(m[1]); k > n {
			n = k
		}
	}
	return n
}

// CheckComfyWorkflow says what is wrong with a workflow, or nil.
func CheckComfyWorkflow(workflow string) error {
	if strings.TrimSpace(workflow) == "" {
		return nil
	}
	var nodes map[string]map[string]any
	if err := json.Unmarshal([]byte(workflow), &nodes); err != nil {
		return fmt.Errorf("workflow không phải JSON dạng API của ComfyUI (dùng “Save (API Format)” / “Export (API)”): %v", err)
	}
	for id, n := range nodes {
		if _, ok := n["class_type"]; !ok {
			return fmt.Errorf("node %s không có class_type — đây là workflow thường, hãy xuất bằng “Save (API Format)”", id)
		}
	}
	if !strings.Contains(workflow, "{prompt}") {
		return errors.New("workflow chưa có chỗ trống {prompt} — đặt {prompt} vào ô text của node CLIPTextEncode")
	}
	return nil
}

// comfy runs the workflow that fits the drawing: the one with reference
// slots when there are references, else the one without (either works
// alone: missing slots repeat the references there are).
func (m *imager) comfy(ctx context.Context, r ImageRequest) (Image, error) {
	withRefs, noRefs := strings.TrimSpace(m.cfg.Workflow), strings.TrimSpace(m.cfg.WorkflowNoRef)
	workflow := withRefs
	if len(r.Refs) == 0 && noRefs != "" || workflow == "" {
		workflow = noRefs
	}
	if workflow == "" {
		return Image{}, errors.New("provider ComfyUI chưa có workflow — dán workflow (API format) vào form provider")
	}
	if err := CheckComfyWorkflow(workflow); err != nil {
		return Image{}, err
	}
	slots := ComfyRefSlots(workflow)
	if slots > 0 && len(r.Refs) == 0 {
		return Image{}, errors.New("workflow ComfyUI cần ảnh tham chiếu nhưng cảnh này không có ảnh nào — thêm một workflow không tham chiếu trong form provider")
	}

	// Upload the references the workflow uses.
	names := make([]string, 0, slots)
	var labels []string
	for i := 0; i < slots && i < len(r.Refs); i++ {
		name, err := m.comfyUpload(ctx, r.Refs[i], i+1)
		if err != nil {
			return Image{}, fmt.Errorf("tải ảnh tham chiếu lên ComfyUI: %w", err)
		}
		names = append(names, name)
		labels = append(labels, fmt.Sprintf("image %d: %s", i+1, r.Refs[i].Label))
	}

	w, h := 1024, 1024
	switch r.Aspect {
	case "portrait":
		w, h = 832, 1216
	case "landscape":
		w, h = 1216, 832
	}
	seed, _ := rand.Int(rand.Reader, big.NewInt(1<<48))
	vals := map[string]any{"width": w, "height": h, "seed": seed.Int64()}
	text := map[string]string{"prompt": r.Prompt, "negative": comfyNegative, "model": m.cfg.Model,
		"ref_labels": strings.Join(labels, "; ")}
	for i := 0; i < slots; i++ {
		text["ref"+strconv.Itoa(i+1)] = names[i%len(names)]
	}

	var graph any
	if err := json.Unmarshal([]byte(workflow), &graph); err != nil {
		return Image{}, err
	}
	graph = fillPlaceholders(graph, vals, text)

	clientID := randomHex(8)
	var queued struct {
		PromptID   string          `json:"prompt_id"`
		NodeErrors json.RawMessage `json:"node_errors"`
	}
	if err := m.postJSON(ctx, "/prompt", "comfyui", map[string]any{"prompt": graph, "client_id": clientID}, &queued); err != nil {
		return Image{}, err
	}
	if queued.PromptID == "" {
		return Image{}, fmt.Errorf("ComfyUI không nhận workflow: %s", headRunes(string(queued.NodeErrors), 300))
	}
	return m.comfyWait(ctx, queued.PromptID)
}

// fillPlaceholders replaces placeholders in every string of the workflow. A
// string that is exactly "{width}", "{height}" or "{seed}" becomes a number.
func fillPlaceholders(v any, vals map[string]any, text map[string]string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			x[k] = fillPlaceholders(c, vals, text)
		}
		return x
	case []any:
		for i, c := range x {
			x[i] = fillPlaceholders(c, vals, text)
		}
		return x
	case string:
		if strings.HasPrefix(x, "{") && strings.HasSuffix(x, "}") {
			if n, ok := vals[x[1:len(x)-1]]; ok {
				return n
			}
		}
		for k, s := range text {
			x = strings.ReplaceAll(x, "{"+k+"}", s)
		}
		for k, n := range vals {
			x = strings.ReplaceAll(x, "{"+k+"}", fmt.Sprint(n))
		}
		return x
	}
	return v
}

func (m *imager) comfyUpload(ctx context.Context, ref RefImage, n int) (string, error) {
	data, err := base64.StdEncoding.DecodeString(ref.B64)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image"; filename="immersive-ref-%s-%d%s"`, randomHex(4), n, extOf(ref.MIME)))
	h.Set("Content-Type", ref.MIME)
	part, err := w.CreatePart(h)
	if err != nil {
		return "", err
	}
	part.Write(data)
	w.WriteField("type", "input")
	w.WriteField("overwrite", "true")
	w.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+"/upload/image", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	var out struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
	}
	if err := m.do(req, "comfyui", &out); err != nil {
		return "", err
	}
	if out.Subfolder != "" {
		return out.Subfolder + "/" + out.Name, nil
	}
	return out.Name, nil
}

type comfyFile struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// comfyWait polls the job's history until it has finished, then fetches the
// picture it saved.
func (m *imager) comfyWait(ctx context.Context, promptID string) (Image, error) {
	for {
		select {
		case <-ctx.Done():
			return Image{}, ctx.Err()
		case <-time.After(time.Second):
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.BaseURL+"/history/"+url.PathEscape(promptID), nil)
		if err != nil {
			return Image{}, err
		}
		var hist map[string]struct {
			Outputs map[string]struct {
				Images []comfyFile `json:"images"`
			} `json:"outputs"`
			Status struct {
				StatusStr string            `json:"status_str"`
				Completed bool              `json:"completed"`
				Messages  []json.RawMessage `json:"messages"`
			} `json:"status"`
		}
		if err := m.do(req, "comfyui", &hist); err != nil {
			return Image{}, err
		}
		job, ok := hist[promptID]
		if !ok {
			continue // still queued or running
		}
		if job.Status.StatusStr == "error" {
			msg := ""
			if n := len(job.Status.Messages); n > 0 {
				msg = string(job.Status.Messages[n-1])
			}
			return Image{}, fmt.Errorf("ComfyUI chạy workflow bị lỗi: %s", headRunes(msg, 400))
		}
		var pick *comfyFile
		for _, o := range job.Outputs {
			for i := range o.Images {
				f := &o.Images[i]
				if pick == nil || (f.Type == "output" && pick.Type != "output") {
					pick = f
				}
			}
		}
		if pick == nil {
			if job.Status.Completed || job.Status.StatusStr == "success" {
				return Image{}, errors.New("workflow chạy xong nhưng không lưu ảnh nào — cần một node SaveImage hoặc PreviewImage")
			}
			continue
		}
		q := url.Values{"filename": {pick.Filename}, "subfolder": {pick.Subfolder}, "type": {pick.Type}}
		return m.download(ctx, m.cfg.BaseURL+"/view?"+q.Encode())
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
