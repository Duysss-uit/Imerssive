"""Document extraction: PDF, EPUB, CBZ, XPS, images (via PyMuPDF) and plain text.

Pages with real text become text pages. Pages that are mostly pictures (manga,
comics, scans) are rendered to JPEG so they can be read by a vision model or
embedded as images.
"""
import os

import pymupdf

TEXT_EXT = {".txt", ".md", ".markdown"}
TEXT_PAGE_CHARS = 3000  # pseudo-page size for plain text files


def extract(path: str, out_dir: str, min_chars: int = 80, max_side: int = 1400) -> dict:
    ext = os.path.splitext(path)[1].lower()
    if ext in TEXT_EXT:
        return {"pages": _extract_text_file(path)}

    os.makedirs(out_dir, exist_ok=True)
    pages = []
    with pymupdf.open(path) as doc:
        for i, page in enumerate(doc):
            text = page.get_text("text").strip()
            has_images = (not doc.is_pdf) or bool(page.get_images(full=False))
            if len(text) >= min_chars or not has_images:
                if text:
                    pages.append({"index": i, "kind": "text", "text": text, "image_path": None})
                continue
            img_path = os.path.join(out_dir, f"p{i:05d}.jpg")
            rect = page.rect
            scale = min(max_side / max(rect.width, rect.height, 1), 3.0)
            pix = page.get_pixmap(matrix=pymupdf.Matrix(scale, scale), alpha=False)
            pix.save(img_path, jpg_quality=85)
            pages.append({"index": i, "kind": "image", "text": text, "image_path": img_path})
    return {"pages": pages}


def _extract_text_file(path: str) -> list:
    with open(path, "rb") as f:
        raw = f.read()
    for enc in ("utf-8-sig", "utf-16", "cp1258", "latin-1"):
        try:
            text = raw.decode(enc)
            break
        except UnicodeDecodeError:
            continue
    pages, buf, size = [], [], 0
    for para in text.replace("\r\n", "\n").split("\n\n"):
        buf.append(para)
        size += len(para)
        if size >= TEXT_PAGE_CHARS:
            pages.append("\n\n".join(buf))
            buf, size = [], 0
    if buf:
        pages.append("\n\n".join(buf))
    return [{"index": i, "kind": "text", "text": t, "image_path": None} for i, t in enumerate(pages) if t.strip()]
