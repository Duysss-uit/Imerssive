package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// RoleImage is the settings role of the provider that draws illustrations.
const RoleImage = "image"

// Image APIs a provider can draw with.
const (
	ImageAPIChat    = "chat"    // Chat Completions with image output (OpenRouter, Gemini image models...)
	ImageAPIImages  = "images"  // OpenAI Images API: /images/generations, /images/edits with references
	ImageAPISDWebUI = "sdwebui" // AUTOMATIC1111 / Forge: /sdapi/v1/txt2img, img2img
)

// ImageAPIFor is the API a provider draws with: the one it set, or the
// usual one for its kind.
func ImageAPIFor(k Kind, set string) string {
	if set != "" {
		return set
	}
	switch k {
	case KindOpenAI:
		return ImageAPIImages
	case KindLocal:
		return ImageAPISDWebUI
	}
	return ImageAPIChat
}

// RefImage is a picture the drawing should follow, with what it shows.
type RefImage struct {
	Label string // e.g. "Mai Linh: her appearance"
	B64   string
	MIME  string
}

type ImageRequest struct {
	Prompt  string
	Refs    []RefImage
	Aspect  string // square | portrait | landscape
	Purpose string // only used in logs
	// Continue marks that Refs[0] is the previous illustration to carry on
	// from (local Stable Diffusion then draws over it).
	Continue bool
}

type Image struct {
	B64  string
	MIME string
}

// Imager draws one picture.
type Imager interface {
	Draw(ctx context.Context, r ImageRequest) (Image, error)
}

// NewImager builds the drawing client for cfg (cfg.ImageAPI picks the API).
func NewImager(cfg Config) (Imager, error) {
	if cfg.Kind == KindAnthropic {
		return nil, errors.New("Anthropic không có model tạo ảnh — chọn provider khác cho vai trò tạo ảnh")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL(cfg.Kind)
	}
	api := ImageAPIFor(cfg.Kind, cfg.ImageAPI)
	if api == ImageAPISDWebUI && (cfg.BaseURL == "" || cfg.BaseURL == DefaultBaseURL(KindLocal)) {
		cfg.BaseURL = "http://127.0.0.1:7860"
	}
	if api == ImageAPIComfyUI && (cfg.BaseURL == "" || cfg.BaseURL == DefaultBaseURL(cfg.Kind)) {
		cfg.BaseURL = DefaultComfyURL
	}
	if cfg.Model == "" && api != ImageAPISDWebUI && api != ImageAPIComfyUI {
		return nil, errors.New("provider tạo ảnh chưa chọn model")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &imager{cfg: cfg, api: api, http: &http.Client{Timeout: 10 * time.Minute}}, nil
}

type imager struct {
	cfg  Config
	api  string
	http *http.Client
}

// refNote lists what each reference shows, for the prompt.
func refNote(refs []RefImage) string {
	if len(refs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\nReference images, in the order attached:")
	for i, r := range refs {
		fmt.Fprintf(&sb, "\n%d. %s", i+1, r.Label)
	}
	sb.WriteString("\nKeep each character's face, hair, outfit and colors as in their reference. Draw a new picture of the scene described above: don't copy the references' layout, panels or speech bubbles.")
	return sb.String()
}

func aspectRatio(a string) string {
	switch a {
	case "portrait":
		return "2:3"
	case "landscape":
		return "3:2"
	}
	return "1:1"
}

func (m *imager) Draw(ctx context.Context, r ImageRequest) (Image, error) {
	if strings.TrimSpace(r.Prompt) == "" {
		return Image{}, errors.New("prompt trống")
	}
	switch m.api {
	case ImageAPIImages:
		return m.images(ctx, r)
	case ImageAPISDWebUI:
		return m.sdwebui(ctx, r)
	case ImageAPIComfyUI:
		return m.comfy(ctx, r)
	}
	return m.chat(ctx, r)
}

func (m *imager) do(req *http.Request, kind string, out any) error {
	if m.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	}
	if m.cfg.Kind == KindOpenRouter {
		req.Header.Set("X-Title", "Immersive")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &statusError{kind: Kind(kind), code: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: câu trả lời không đọc được: %w", kind, err)
	}
	return nil
}

func (m *imager) postJSON(ctx context.Context, path, kind string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return m.do(req, kind, out)
}

// chat asks a chat model with image output (OpenRouter's "modalities"); each
// reference goes right after the line saying what it shows.
func (m *imager) chat(ctx context.Context, r ImageRequest) (Image, error) {
	content := []map[string]any{{"type": "text", "text": r.Prompt + fmt.Sprintf("\n\nAspect ratio %s. Reply with the image only.", aspectRatio(r.Aspect))}}
	for i, ref := range r.Refs {
		content = append(content,
			map[string]any{"type": "text", "text": fmt.Sprintf("Reference %d: %s", i+1, ref.Label)},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + ref.MIME + ";base64," + ref.B64}})
	}
	if len(r.Refs) > 0 {
		content = append(content, map[string]any{"type": "text", "text": "Keep each character's face, hair, outfit and colors as in their reference. Draw a new picture of the scene described above: don't copy the references' layout, panels or speech bubbles."})
	}
	body := map[string]any{
		"model":        m.cfg.Model,
		"messages":     []map[string]any{{"role": "user", "content": content}},
		"modalities":   []string{"image", "text"},
		"image_config": map[string]any{"aspect_ratio": aspectRatio(r.Aspect)},
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
				Images  []struct {
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"images"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := m.postJSON(ctx, "/chat/completions", "image", body, &out); err != nil {
		return Image{}, err
	}
	if len(out.Choices) == 0 {
		return Image{}, errors.New("model không trả về ảnh nào")
	}
	msg := out.Choices[0].Message
	for _, im := range msg.Images {
		if img, ok := dataURI(im.ImageURL.URL); ok {
			return img, nil
		}
		if strings.HasPrefix(im.ImageURL.URL, "http") {
			return m.download(ctx, im.ImageURL.URL)
		}
	}
	// Some servers put the image in the content parts instead.
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	var text string
	if json.Unmarshal(msg.Content, &parts) == nil {
		for _, p := range parts {
			if img, ok := dataURI(p.ImageURL.URL); ok {
				return img, nil
			}
			text += p.Text
		}
	} else {
		json.Unmarshal(msg.Content, &text)
	}
	if i := strings.Index(text, "data:image/"); i >= 0 {
		end := strings.IndexAny(text[i:], ") \n\"")
		if end < 0 {
			end = len(text) - i
		}
		if img, ok := dataURI(text[i : i+end]); ok {
			return img, nil
		}
	}
	if text = strings.TrimSpace(text); text != "" {
		return Image{}, fmt.Errorf("model trả lời bằng chữ thay vì ảnh (bị từ chối, hoặc model này không tạo được ảnh): %s", headRunes(text, 2000))
	}
	return Image{}, errors.New("model không trả về ảnh nào (model này có tạo được ảnh không?)")
}

// images uses the OpenAI Images API: generations, or edits when there are
// references (gpt-image-1 takes several).
func (m *imager) images(ctx context.Context, r ImageRequest) (Image, error) {
	size := map[string]string{"portrait": "1024x1536", "landscape": "1536x1024"}[r.Aspect]
	if size == "" {
		size = "1024x1024"
	}
	dalle := strings.HasPrefix(m.cfg.Model, "dall-e")
	if dalle && size != "1024x1024" {
		size = map[string]string{"1024x1536": "1024x1792", "1536x1024": "1792x1024"}[size]
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
	}
	if len(r.Refs) == 0 || dalle {
		body := map[string]any{"model": m.cfg.Model, "prompt": r.Prompt, "size": size, "n": 1}
		if dalle {
			body["response_format"] = "b64_json"
		}
		if err := m.postJSON(ctx, "/images/generations", "image", body, &out); err != nil {
			return Image{}, err
		}
	} else {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		w.WriteField("model", m.cfg.Model)
		w.WriteField("prompt", r.Prompt+refNote(r.Refs))
		w.WriteField("size", size)
		w.WriteField("n", "1")
		for i, ref := range r.Refs {
			data, err := base64.StdEncoding.DecodeString(ref.B64)
			if err != nil {
				continue
			}
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image[]"; filename="ref%d%s"`, i+1, extOf(ref.MIME)))
			h.Set("Content-Type", ref.MIME)
			part, err := w.CreatePart(h)
			if err != nil {
				return Image{}, err
			}
			part.Write(data)
		}
		w.Close()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+"/images/edits", &buf)
		if err != nil {
			return Image{}, err
		}
		req.Header.Set("Content-Type", w.FormDataContentType())
		if err := m.do(req, "image", &out); err != nil {
			// Many OpenAI-compatible servers only have /images/generations:
			// draw from the prompt alone rather than not at all.
			var se *statusError
			if !errors.As(err, &se) || !editsUnsupported(se.code) {
				return Image{}, err
			}
			log.Printf("image: %s không nhận ảnh tham chiếu (/images/edits → %d), vẽ không tham chiếu", m.cfg.Model, se.code)
			noRefs := r
			noRefs.Refs = nil
			return m.images(ctx, noRefs)
		}
	}
	if len(out.Data) == 0 {
		return Image{}, errors.New("model không trả về ảnh nào")
	}
	if out.Data[0].B64 != "" {
		return Image{B64: out.Data[0].B64, MIME: "image/png"}, nil
	}
	return m.download(ctx, out.Data[0].URL)
}

// sdwebui draws with a local AUTOMATIC1111/Forge server. It can't follow
// character references, so it draws from the prompt alone, or over the
// previous illustration when the scene carries on from it.
func (m *imager) sdwebui(ctx context.Context, r ImageRequest) (Image, error) {
	w, h := 768, 768
	switch r.Aspect {
	case "portrait":
		w, h = 640, 960
	case "landscape":
		w, h = 960, 640
	}
	body := map[string]any{"prompt": r.Prompt, "negative_prompt": "lowres, bad anatomy, bad hands, text, watermark, speech bubble, blurry",
		"width": w, "height": h, "steps": 28, "cfg_scale": 6}
	if m.cfg.Model != "" {
		body["override_settings"] = map[string]any{"sd_model_checkpoint": m.cfg.Model}
	}
	path := "/sdapi/v1/txt2img"
	if r.Continue && len(r.Refs) > 0 {
		path = "/sdapi/v1/img2img"
		body["init_images"] = []string{r.Refs[0].B64}
		body["denoising_strength"] = 0.6
	}
	var out struct {
		Images []string `json:"images"`
	}
	if err := m.postJSON(ctx, path, "stable diffusion", body, &out); err != nil {
		return Image{}, err
	}
	if len(out.Images) == 0 {
		return Image{}, errors.New("stable diffusion không trả về ảnh nào")
	}
	return Image{B64: out.Images[0], MIME: "image/png"}, nil
}

func (m *imager) download(ctx context.Context, url string) (Image, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Image{}, err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return Image{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return Image{}, err
	}
	if resp.StatusCode/100 != 2 {
		return Image{}, fmt.Errorf("tải ảnh: HTTP %d", resp.StatusCode)
	}
	mt := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(mt, "image/") {
		mt = http.DetectContentType(b)
	}
	return Image{B64: base64.StdEncoding.EncodeToString(b), MIME: mt}, nil
}

// dataURI reads "data:image/png;base64,...".
func dataURI(s string) (Image, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "data:image/") {
		return Image{}, false
	}
	head, data, ok := strings.Cut(s, ",")
	if !ok || !strings.HasSuffix(head, ";base64") {
		return Image{}, false
	}
	return Image{B64: data, MIME: strings.TrimSuffix(strings.TrimPrefix(head, "data:"), ";base64")}, true
}

func extOf(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	}
	return ".png"
}

func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ImageModels lists what an image provider can draw with: ComfyUI's
// checkpoints, SD WebUI's models, or the API's models.
func ImageModels(ctx context.Context, cfg Config) ([]string, error) {
	if cfg.Model == "" {
		cfg.Model = "list" // NewImager requires one for API kinds
	}
	im, err := NewImager(cfg)
	if err != nil {
		return nil, err
	}
	m := im.(*imager)
	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.BaseURL+path, nil)
		if err != nil {
			return err
		}
		return m.do(req, m.api, out)
	}
	switch m.api {
	case ImageAPIComfyUI:
		var info map[string]struct {
			Input struct {
				Required map[string][]json.RawMessage `json:"required"`
			} `json:"input"`
		}
		if err := get("/object_info/CheckpointLoaderSimple", &info); err != nil {
			return nil, err
		}
		spec := info["CheckpointLoaderSimple"].Input.Required["ckpt_name"]
		if len(spec) == 0 {
			return nil, errors.New("ComfyUI không cho biết danh sách checkpoint")
		}
		var names []string
		if json.Unmarshal(spec[0], &names) == nil { // ["a.safetensors", ...]
			return names, nil
		}
		var opts struct { // "COMBO", {"options": [...]}
			Options []string `json:"options"`
		}
		if len(spec) > 1 && json.Unmarshal(spec[1], &opts) == nil {
			return opts.Options, nil
		}
		return nil, errors.New("không đọc được danh sách checkpoint của ComfyUI")
	case ImageAPISDWebUI:
		var models []struct {
			Title string `json:"title"`
		}
		if err := get("/sdapi/v1/sd-models", &models); err != nil {
			return nil, err
		}
		out := make([]string, len(models))
		for i, x := range models {
			out[i] = x.Title
		}
		return out, nil
	}
	return newOpenAICompat(cfg).ListModels(ctx)
}

// editsUnsupported: the server has no (working) /images/edits for this model.
func editsUnsupported(code int) bool {
	switch code {
	case 400, 404, 405, 415, 422, 501:
		return true
	}
	return false
}
