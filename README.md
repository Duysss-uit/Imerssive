# Immersive

Ứng dụng desktop nhập vai với AI (kiểu c.ai). Nhân vật và bối cảnh được dựng từ **chính bộ truyện của bạn**: PDF, EPUB, truyện tranh CBZ, ảnh scan…
Nội dung truyện **không** bị nhồi hết vào context của model. App giữ một **bản đồ truyện** ngắn (mỗi chương vài câu) cùng hồ sơ nhân vật, còn nguyên văn và lời thoại thì embedding vào RAG. Khi chat, model đọc bản đồ rồi **tự tra** những gì cảnh truyện cần.

## Kiến trúc

```
┌──────────────── Immersive.exe (Go + Wails, ~50 MB RAM) ────────────────┐
│  UI: HTML/CSS + 1 file JS mỏng (WebView2 có sẵn trên Windows)          │
│                                                                        │
│  internal/app      facade bind cho UI                                  │
│  internal/llm      Provider interface ─┬─ anthropic.go (Claude SDK)    │
│                                        └─ openai.go (OpenAI, OpenRouter,│
│                                           NVIDIA NIM, Ollama/LM Studio) │
│  internal/ingest   pipeline: trích xuất → đọc tranh → embed → bản đồ   │
│  internal/lore     bản đồ truyện, hồ sơ, công cụ tra cứu cho model     │
│  internal/chat     engine nhập vai, sổ thay đổi, nén ký ức dài hạn     │
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
│  retrieval.py  LangChain: vector Chroma + BM25, ghép bằng EnsembleRetriever │
└────────────────────────────────────────────────────────────────────────┘
```

**Về tài nguyên:** Python và model embedding **chỉ nằm trong RAM khi đang cần**: lúc xử lý tài liệu, hoặc lúc truy xuất trong một lượt chat. Sau `worker.idle_minutes` phút rảnh (mặc định 5), worker tự tắt.
Đo thực tế khi rảnh: tiến trình Go khoảng 53 MB và WebView2 khoảng 158 MB (private memory), không có tiến trình Python nào.

### Pipeline xử lý tài liệu

```
PDF/EPUB/CBZ ─► trích xuất từng trang
                ├─ trang chữ ──────────────────────────────────────┐
                └─ trang tranh ─┬─ vision đọc từng trang (1 trang/lần,┤  tắt suy nghĩ, ảnh thu nhỏ)
                                │  hành động + lời thoại + SFX)      │
                                └─ giữ ảnh trang để embed bằng CLIP ở bước 2 (0 token)
                                                                    ▼
                     văn bản + bản chép từng trang tranh  ◄─ bước 1: đọc (mọi tài liệu trong hàng đợi)
                                                                    ▼
                     embedding toàn bộ: đoạn văn + ảnh trang  ◄─ bước 2: chờ mọi tài liệu đọc xong
                                                                    ▼
                     chia chương (theo tiêu đề "Chương 12", "Chapter IV", "第十二章"…, không có thì mỗi file một chương)
                     đọc từng chương theo từng đoạn vừa context của model,  ◄─ bước 3: chờ mọi tài liệu embedding xong
                     bản tóm tắt chương được cập nhật sau mỗi đoạn
                                                                    ▼
                     bản đồ truyện (mỗi chương vài câu + ai xuất hiện) + hồ sơ nhân vật/địa danh gộp qua mọi chương
                                                                    ▼
                     lưu vào SQLite (bản đồ) và Chroma kèm metadata
                     {kb_id, source_id, level: raw|section|entity, pages, name}
```

Không lần gọi model nào phải ôm cả một chương hay cả bộ truyện: mỗi provider có **context window** (app tự hỏi LM Studio/OpenRouter, hoặc bạn nhập trong Cài đặt → Provider), và mọi prompt (đoạn chương, ghi chú gộp hồ sơ, lịch sử chat, kết quả tra cứu) được cắt cho vừa con số đó, chừa chỗ cho phần suy nghĩ và câu trả lời.

Mỗi bước chỉ bắt đầu khi mọi tài liệu đang chờ của bộ truyện đã xong bước trước. Thứ tự truyện là thứ tự thêm vào; nhiều file chọn cùng lúc được xếp theo tên tự nhiên (Chương 2 trước Chương 10). Tài liệu thêm sau **nối tiếp bản đồ**: các chương mới được đọc với bản đồ các chương trước làm ngữ cảnh, hồ sơ cũ được cập nhật. Xử lý lại một tài liệu nằm giữa truyện, hoặc bấm “↻ Lập lại bản đồ”, sẽ lập lại cả bộ từ đầu. Bộ truyện nạp bằng bản cũ (có “tổng quan” thay vì bản đồ) cần bấm nút này một lần.

### Một lượt chat

1. System prompt gồm thẻ nhân vật/bối cảnh, **bản đồ truyện** (rút gọn nếu dài) và **sổ thay đổi** của cuộc trò chuyện.
2. Model có ba công cụ: `get_character` (hồ sơ), `search_story` (tìm theo nghĩa và từ khoá) và `read_chapter` (đọc nguyên văn/lời thoại của một chương, theo trang). Nó tự tra tối đa 4 lần trước khi trả lời; UI hiện “Đang tra cứu…”. Tìm kiếm chạy bằng **LangChain** trong worker: vector Chroma + BM25 (bắt chính xác tên riêng), ghép bằng `EnsembleRetriever`.
3. Model không gọi được công cụ thì app tự tìm như trước: kết quả đặt trong thẻ `<memory>` ở lượt user mới nhất (không nằm trong system prompt, không lưu vào lịch sử).
4. **Sổ thay đổi:** cứ `chat.divergence_every` lượt (mặc định 3), model tóm tắt ghi lại những gì câu chuyện đã khác nguyên tác vì lựa chọn của người chơi (ai chết, quan hệ đổi, đang ở đâu…). Sổ này đứng trên nguyên tác: model tra nguyên tác rồi điều chỉnh theo sổ. Xem, sửa hoặc cập nhật ngay ở nút ⋯ trong màn hình chat.
5. Khi số tin nhắn vượt `chat.window`, nửa cũ nhất được tóm tắt thành ghi chú ký ức, embed vào RAG (`level=memory`) rồi rời khỏi context.

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
   - `vision`: vision LLM chép lại từng trang (mỗi lần gọi một trang, nhiều trang chạy song song): ai xuất hiện, hành động từng khung, lời thoại nguyên văn kèm người nói, hiệu ứng âm thanh. Vision luôn chạy với suy nghĩ tắt, ảnh được thu nhỏ còn cạnh dài `ingest.vision_max_side` (mặc định 1024 px). Chính xác nhất, chỉ tốn token **một lần** lúc xử lý.
   - `embed`: chỉ embed ảnh trang bằng CLIP, **không tốn token**. Không tóm tắt được, nhưng có thể gửi kèm trang liên quan khi chat (bật `chat.attach_images`).
   - `both`: làm cả hai.
   Ở thẻ **Model xử lý & embedding** của từng bộ truyện, có thể chọn model tóm tắt, model vision, và embedding: local (fastembed, miễn phí) hoặc qua API (OpenAI, OpenRouter, NVIDIA, Ollama/LM Studio). Đổi embedding thì app tự embedding lại văn bản đã lưu, không gọi lại LLM.
4. Dùng ô **Thử truy xuất RAG** để xem model sẽ “nhớ” được gì.
5. **Nhân vật / Bối cảnh:** chọn bộ truyện, nhập tên, chọn model rồi bấm ✨ Tạo. Model chỉ nhận bản đồ truyện và danh sách tên có trong truyện, rồi **tự tra** những gì nó cần bằng các công cụ `get_character`, `search_story`, `read_chapter` (tối đa 5 lần; các lần tra được ghi trong model log). Model không gọi được công cụ thì liệt kê trước những gì cần tìm, app tìm giúp rồi đưa kết quả cho model. Sau đó bạn sửa lại rồi lưu.
6. Bấm vào thẻ nhân vật để tiếp tục cuộc trò chuyện gần nhất (hoặc bắt đầu mới). Model của từng cuộc trò chuyện đổi được ở góc trên bên phải màn hình chat; nút ⋯ mở thông tin nhân vật, lịch sử và “Chat mới”.
7. **Hoá thân vào truyện:** ở tab Bối cảnh, nhập ý tưởng rồi bấm ✨ Tạo. AI dựng bối cảnh và trích xuất các nhân vật có mặt từ bộ truyện (nếu tự viết bối cảnh thì bấm “✨ Trích xuất từ truyện”). Bấm “Lưu & nhập vai”, rồi chọn vào vai một nhân vật trong danh sách hoặc tự tạo nhân vật. AI làm người dẫn truyện, điều khiển thế giới và mọi nhân vật còn lại. Vẫn có thể chọn “Chat với một nhân vật” để chat 1-1 trong bối cảnh đó.
8. **Chơi ngay:** nút ▶ Chơi ngay (trong Thư viện, tab Bối cảnh hoặc menu + Tạo) đưa bạn vào truyện mà không cần tạo bối cảnh. Chọn bộ truyện, có thể gõ một dòng muốn bắt đầu từ đâu, rồi chọn vai từ danh sách nhân vật đã có sẵn trong hồ sơ của bộ truyện (không gọi model) hoặc tự tạo nhân vật. Người dẫn truyện tự viết đoạn mở đầu từ dữ liệu trong truyện.

## Dữ liệu

Tất cả nằm trong `%APPDATA%\Immersive` (đổi được bằng biến `IMMERSIVE_DATA_DIR`):
`app.db` (SQLite), `chroma/` (vector), `sources/` (bản sao tài liệu), `pages/` (ảnh trang truyện tranh), `models/`, `logs/`.

**Prompt:** mọi prompt gửi cho model nằm trong các file `.md` ở thư mục `prompts/` cạnh `Immersive.exe` (nếu thư mục đó không ghi được thì ở `%APPDATA%\Immersive\prompts`). Sửa file là có hiệu lực ở lần gọi model tiếp theo; xoá một file để quay về bản mặc định. File bạn chưa sửa được tự nâng lên bản mặc định mới khi cập nhật app; file đã sửa thì giữ nguyên. Đầu mỗi file có ghi chú (không gửi cho model) giải thích công dụng và các biến `{title}`, `{text}`…; `{{char}}`/`{{user}}` là tên nhân vật và người chơi. Bản mặc định nằm ở `app/internal/prompts/defaults/`. Nút trong Cài đặt → Prompt mở thư mục này.

**Log:** mỗi ngày một file trong `logs/`. `app-YYYY-MM-DD.log` ghi hoạt động (lưu/xoá, gửi tin nhắn, tiến độ xử lý tài liệu, lỗi, kèm một dòng tóm tắt cho mỗi lần gọi model). `model-YYYY-MM-DD.log` ghi đầy đủ từng lần gọi model: mục đích, provider/model, thời gian, system prompt, các tin nhắn gửi đi (ảnh chỉ ghi kích thước) và toàn bộ nội dung model trả về. Mức chi tiết chỉnh ở Cài đặt → Nhật ký (đầy đủ / chỉ câu trả lời / tắt). Log quá 14 ngày tự xoá; API key không được ghi.

## Mở rộng

- **Thêm provider mới:** viết một kiểu implement `llm.Provider` (gồm `Stream`, `ListModels`, `SupportsVision`) rồi thêm một nhánh trong `llm.New`. Provider nào tương thích OpenAI thì chỉ cần thêm `Kind` và base URL.
- **Đổi model embedding local:** sửa trong Cài đặt. Mỗi model có collection riêng (`text-<model>`), nên vector với số chiều khác nhau không bị trộn; tài liệu cũ cần “Xử lý lại”. Tiếng Việt chất lượng cao hơn: thử `intfloat/multilingual-e5-large` (nặng hơn khoảng 2 GB).
- **Đổi vector DB:** chỉ cần sửa `worker/imm_worker/vectors.py`, vì Go chỉ biết tới các method RPC `store.*`.

## Test

```powershell
cd app
go test ./...                                   # unit test (chia chương, bản đồ, tra cứu, sổ thay đổi, prompt…)
$env:IMM_WORKER_DIR="..\worker"; $env:IMM_TEST_PDF="C:\path\sample.pdf"
go test ./internal/integration -v               # end-to-end: Python worker thật + LLM giả lập
```

## Giới hạn hiện tại

- CLIP chỉ hiểu tốt truy vấn tiếng Anh, nên chế độ `embed` tìm trang theo câu hỏi tiếng Việt khá yếu. Chế độ `vision` thì không bị ảnh hưởng.
- Model tóm tắt phải trả về JSON. Nếu model yếu trả sai định dạng, app vẫn giữ phần tóm tắt dạng văn xuôi nhưng sẽ mất danh sách nhân vật của đoạn đó.
- Lập bản đồ đi tuần tự từng chương (chương sau cần bản đồ chương trước), nên với model nhỏ context sẽ có nhiều lượt gọi: khoảng (số ký tự / đoạn vừa context) lần. Nên dùng model rẻ cho vai “Tóm tắt”.
- Chat tự tra cứu cần model biết gọi công cụ (tool calling). Model không biết thì app tự truy xuất thay, kém chính xác hơn.
- Context window đoán theo ~2 ký tự/token cho an toàn với tiếng Việt/Nhật. Nếu server không cho biết, app dùng 4096 cho local và 32768 cho API: hãy nhập con số thật để model đọc được đoạn dài hơn.
