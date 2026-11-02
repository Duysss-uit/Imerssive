# Immersive

Ứng dụng desktop nhập vai với AI (kiểu c.ai). Nhân vật và bối cảnh được dựng từ **chính bộ truyện của bạn**: PDF, EPUB, truyện tranh CBZ, ảnh scan…
Nội dung truyện **không** bị nhồi hết vào context của model. Nó được tóm tắt, embedding rồi lưu vào hệ thống RAG, và model chỉ lấy ra phần liên quan ở mỗi lượt chat.

## Kiến trúc

```
┌──────────────── Immersive.exe (Go + Wails, ~50 MB RAM) ────────────────┐
│  UI: HTML/CSS + 1 file JS mỏng (WebView2 có sẵn trên Windows)          │
│                                                                        │
│  internal/app      facade bind cho UI                                  │
│  internal/llm      Provider interface ─┬─ anthropic.go (Claude SDK)    │
│                                        └─ openai.go (OpenAI, OpenRouter,│
│                                           NVIDIA NIM, Ollama/LM Studio) │
│  internal/ingest   pipeline: trích xuất → đọc tranh → tóm tắt → embed  │
│  internal/chat     engine nhập vai + nén ký ức dài hạn                 │
│  internal/rag      facade vector store                                 │
│  internal/store    SQLite (pure Go)      internal/secrets  keyring     │
│  internal/worker   quản lý tiến trình Python (bật khi cần, tự tắt)     │
└───────────────────────────────┬────────────────────────────────────────┘
                                │ JSON-RPC qua stdin/stdout
┌───────────────────────────────▼────────────────────────────────────────┐
│ worker/imm_worker (Python, chỉ chạy khi xử lý tài liệu / truy xuất)    │
│  docs.py     PyMuPDF: PDF/EPUB/CBZ/ảnh/TXT → trang chữ + ảnh trang     │
│  embed.py    fastembed (ONNX, không PyTorch): text đa ngôn ngữ + CLIP  │
│  vectors.py  Chroma (lưu trên đĩa)                                     │
└────────────────────────────────────────────────────────────────────────┘
```

**Về tài nguyên:** Python và model embedding **chỉ nằm trong RAM khi đang cần**: lúc xử lý tài liệu, hoặc lúc truy xuất trong một lượt chat. Sau `worker.idle_minutes` phút rảnh (mặc định 5), worker tự tắt.
Đo thực tế khi rảnh: tiến trình Go khoảng 53 MB và WebView2 khoảng 158 MB (private memory), không có tiến trình Python nào.

### Pipeline xử lý tài liệu

```
PDF/EPUB/CBZ ─► trích xuất từng trang
                ├─ trang chữ ─────────────────────────────────┐
                └─ trang tranh ─┬─ vision LLM đọc + mô tả ─────┤  (tốn token 1 lần)
                                └─ embed ảnh bằng CLIP ──────► vector ảnh (0 token)
                                                              ▼
                     đoạn gốc (raw) ─► tóm tắt từng phần + rút thực thể (JSON)
                                                              ▼
                     hồ sơ nhân vật/địa danh (gộp qua các phần) ─► tổng quan toàn tác phẩm
                                                              ▼
                     tất cả được embedding và lưu vào Chroma kèm metadata
                     {kb_id, source_id, level: raw|section|entity|overview, pages, name}
```

### Một lượt chat

1. Câu truy vấn được ghép từ tin nhắn mới nhất và câu trả lời trước đó, rồi đem tìm trong RAG (lọc theo các bộ truyện gắn với nhân vật/bối cảnh, và theo ký ức của chính cuộc trò chuyện).
2. Kết quả được đặt trong thẻ `<memory>` ở **lượt user mới nhất**, không nằm trong system prompt và cũng không lưu vào lịch sử. Nhờ vậy system prompt (thẻ nhân vật) giữ nguyên từng byte và được prompt cache.
3. Khi số tin nhắn vượt `chat.window`, nửa cũ nhất được tóm tắt thành ghi chú ký ức, embed vào RAG (`level=memory`) rồi rời khỏi context.

## Cài đặt

Yêu cầu: Windows 10/11 (có WebView2), [Go 1.24+](https://go.dev/dl/), [uv](https://docs.astral.sh/uv/).

```powershell
winget install GoLang.Go astral-sh.uv
.\scripts\setup-worker.ps1     # tạo worker\.venv (tự tải CPython chính thức, không đụng Python hệ thống)
.\scripts\build.ps1            # ra app\build\bin\Immersive.exe
```

Không cần Node.js hay Wails CLI. Nếu muốn có hot-reload khi sửa UI thì cài thêm Wails CLI và chạy `wails dev` trong `app/`.

> Python của MSYS2/MinGW **không** dùng được vì không có wheel cho onnxruntime/chromadb. Script đã dùng uv để tránh vấn đề này.

Lần đầu xử lý tài liệu, app sẽ tải model embedding (khoảng 220 MB cho text, khoảng 350 MB cho CLIP) về `%APPDATA%\Immersive\models`.

## Sử dụng

1. **Cài đặt → Provider:** thêm Claude, OpenAI, OpenRouter, NVIDIA NIM hoặc Local (Ollama `http://localhost:11434/v1`, LM Studio `http://localhost:1234/v1`). API key được lưu trong **Windows Credential Manager**, không ghi vào database. Nút “Tải danh sách” lấy danh sách model từ endpoint.
2. **Model mặc định:** chọn model mạnh cho chat, model rẻ cho tóm tắt, và một model có vision để đọc truyện tranh. Đây chỉ là mặc định: mỗi thao tác bên dưới đều cho chọn model riêng.
3. **Thư viện:** tạo bộ truyện rồi thêm tài liệu. Với truyện tranh có 3 chế độ:
   - `vision`: vision LLM đọc thoại và mô tả từng trang (mặc định 4 trang mỗi lần gọi). Sau đó nội dung được tóm tắt như truyện chữ. Chính xác nhất, chỉ tốn token **một lần** lúc xử lý.
   - `embed`: chỉ embed ảnh trang bằng CLIP, **không tốn token**. Không tóm tắt được, nhưng có thể gửi kèm trang liên quan khi chat (bật `chat.attach_images`).
   - `both`: làm cả hai.
   Ở thẻ **Model xử lý & embedding** của từng bộ truyện, có thể chọn model tóm tắt, model vision, và embedding: local (fastembed, miễn phí) hoặc qua API (OpenAI, OpenRouter, NVIDIA, Ollama/LM Studio). Đổi embedding thì app tự embedding lại văn bản đã lưu, không gọi lại LLM.
4. Dùng ô **Thử truy xuất RAG** để xem model sẽ “nhớ” được gì.
5. **Nhân vật / Bối cảnh:** chọn bộ truyện, nhập tên, chọn model rồi bấm ✨ Tạo. AI dựng thẻ nhân vật từ hồ sơ trong RAG, sau đó bạn sửa lại rồi lưu.
6. Bấm vào thẻ nhân vật để tiếp tục cuộc trò chuyện gần nhất (hoặc bắt đầu mới). Model của từng cuộc trò chuyện đổi được ở góc trên bên phải màn hình chat; nút ⋯ mở thông tin nhân vật, lịch sử và “Chat mới”.
7. **Hoá thân vào truyện:** ở tab Bối cảnh, nhập ý tưởng rồi bấm ✨ Tạo. AI dựng bối cảnh và trích xuất các nhân vật có mặt từ bộ truyện (nếu tự viết bối cảnh thì bấm “✨ Trích xuất từ truyện”). Bấm “Lưu & nhập vai”, rồi chọn vào vai một nhân vật trong danh sách hoặc tự tạo nhân vật. AI làm người dẫn truyện, điều khiển thế giới và mọi nhân vật còn lại. Vẫn có thể chọn “Chat với một nhân vật” để chat 1-1 trong bối cảnh đó.

## Dữ liệu

Tất cả nằm trong `%APPDATA%\Immersive` (đổi được bằng biến `IMMERSIVE_DATA_DIR`):
`app.db` (SQLite), `chroma/` (vector), `sources/` (bản sao tài liệu), `pages/` (ảnh trang truyện tranh), `models/`, `logs/`.

**Log:** mỗi ngày một file trong `logs/`. `app-YYYY-MM-DD.log` ghi hoạt động (lưu/xoá, gửi tin nhắn, tiến độ xử lý tài liệu, lỗi, kèm một dòng tóm tắt cho mỗi lần gọi model). `model-YYYY-MM-DD.log` ghi đầy đủ từng lần gọi model: mục đích, provider/model, thời gian, system prompt, các tin nhắn gửi đi (ảnh chỉ ghi kích thước) và toàn bộ nội dung model trả về. Mức chi tiết chỉnh ở Cài đặt → Nhật ký (đầy đủ / chỉ câu trả lời / tắt). Log quá 14 ngày tự xoá; API key không được ghi.

## Mở rộng

- **Thêm provider mới:** viết một kiểu implement `llm.Provider` (gồm `Stream`, `ListModels`, `SupportsVision`) rồi thêm một nhánh trong `llm.New`. Provider nào tương thích OpenAI thì chỉ cần thêm `Kind` và base URL.
- **Đổi model embedding local:** sửa trong Cài đặt. Mỗi model có collection riêng (`text-<model>`), nên vector với số chiều khác nhau không bị trộn; tài liệu cũ cần “Xử lý lại”. Tiếng Việt chất lượng cao hơn: thử `intfloat/multilingual-e5-large` (nặng hơn khoảng 2 GB).
- **Đổi vector DB:** chỉ cần sửa `worker/imm_worker/vectors.py`, vì Go chỉ biết tới các method RPC `store.*`.

## Test

```powershell
cd app
go test ./...                                   # unit test (chia đoạn, tách trang, parse JSON)
$env:IMM_WORKER_DIR="..\worker"; $env:IMM_TEST_PDF="C:\path\sample.pdf"
go test ./internal/integration -v               # end-to-end: Python worker thật + LLM giả lập
```

## Giới hạn hiện tại

- CLIP chỉ hiểu tốt truy vấn tiếng Anh, nên chế độ `embed` tìm trang theo câu hỏi tiếng Việt khá yếu. Chế độ `vision` thì không bị ảnh hưởng.
- Hồ sơ thực thể chỉ được gộp trong phạm vi một tài liệu. Nếu một bộ truyện chia thành nhiều file, mỗi file sẽ có hồ sơ riêng (truy xuất vẫn tìm thấy hết).
- Model tóm tắt phải trả về JSON. Nếu model yếu trả sai định dạng, app vẫn giữ phần tóm tắt dạng văn xuôi nhưng sẽ mất danh sách thực thể của phần đó.
- Tài liệu dài tốn nhiều lượt gọi LLM: khoảng (số ký tự / 16.000) lần gọi tóm tắt, cộng thêm lượt gộp hồ sơ và tổng quan. Nên dùng model rẻ cho vai “Tóm tắt”.
