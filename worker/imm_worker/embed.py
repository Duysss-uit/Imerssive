"""Embedding models (fastembed / ONNX runtime, no PyTorch), loaded lazily.

Text model: multilingual sentence embeddings (Vietnamese, English, Japanese...).
Image model: CLIP, so comic pages can be found from a text query without
spending any LLM tokens. The CLIP text tower embeds the query.
"""
import os
import threading

from fastembed import ImageEmbedding, TextEmbedding

TEXT_MODEL = os.environ.get("IMM_TEXT_MODEL") or "sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2"
IMAGE_MODEL = os.environ.get("IMM_IMAGE_MODEL") or "Qdrant/clip-ViT-B-32-vision"
IMAGE_TEXT_MODEL = os.environ.get("IMM_IMAGE_TEXT_MODEL") or IMAGE_MODEL.replace("-vision", "-text")
CACHE_DIR = os.environ.get("IMM_MODELS_DIR") or None

_lock = threading.Lock()
_models: dict = {}


def _get(key: str, factory):
    with _lock:
        if key not in _models:
            _models[key] = factory()
        return _models[key]


def _text():
    return _get("text", lambda: TextEmbedding(model_name=TEXT_MODEL, cache_dir=CACHE_DIR))


def _image():
    return _get("image", lambda: ImageEmbedding(model_name=IMAGE_MODEL, cache_dir=CACHE_DIR))


def _image_text():
    return _get("image_text", lambda: TextEmbedding(model_name=IMAGE_TEXT_MODEL, cache_dir=CACHE_DIR))


def passages(texts: list) -> list:
    return [v.tolist() for v in _text().passage_embed(texts)]


def query(text: str) -> list:
    return next(iter(_text().query_embed(text))).tolist()


def images(paths: list) -> list:
    return [v.tolist() for v in _image().embed(paths)]


def image_query(text: str) -> list:
    return next(iter(_image_text().embed([text]))).tolist()
