"""Chroma vector store. One collection per (kind, embedding model) so switching
models never mixes vector dimensions.

Text calls take an optional `space`: the name of an API embedding model whose
vectors the caller computes and passes in (`embeddings` / `embedding`). Without
it, the local fastembed model embeds and its collection is used."""
import os
import re
import threading

import chromadb
from chromadb.config import Settings

from . import embed

CHROMA_DIR = os.environ.get("IMM_CHROMA_DIR") or os.path.join(os.getcwd(), ".chroma")

_lock = threading.Lock()
_client = None


def _db():
    global _client
    with _lock:
        if _client is None:
            _client = chromadb.PersistentClient(path=CHROMA_DIR, settings=Settings(anonymized_telemetry=False))
        return _client


def _name(kind: str, model: str) -> str:
    slug = re.sub(r"[^a-zA-Z0-9]+", "-", model).strip("-").lower()
    return f"{kind}-{slug}"[:60].strip("-")


def _col(kind: str, space: str = ""):
    model = space or (embed.TEXT_MODEL if kind == "text" else embed.IMAGE_MODEL)
    return _db().get_or_create_collection(_name(kind, model), metadata={"hnsw:space": "cosine"}, embedding_function=None)


def _hits(ids, docs, metas, dists=None) -> list:
    out = []
    for i, id_ in enumerate(ids):
        out.append({
            "id": id_,
            "text": docs[i] if docs else "",
            "metadata": metas[i] if metas else {},
            "distance": float(dists[i]) if dists else 0.0,
        })
    return out


def _clean_meta(m: dict) -> dict:
    # Chroma accepts only str/int/float/bool values
    return {k: v for k, v in (m or {}).items() if isinstance(v, (str, int, float, bool))}


def add_texts(items: list, space: str = "", embeddings: list = None) -> dict:
    if not items:
        return {"added": 0}
    if space and embeddings is None:
        raise ValueError("API embedding space needs precomputed embeddings")
    vecs = embeddings if embeddings is not None else embed.passages([it["text"] for it in items])
    _col("text", space).upsert(
        ids=[it["id"] for it in items],
        embeddings=vecs,
        documents=[it["text"] for it in items],
        metadatas=[_clean_meta(it.get("metadata")) for it in items],
    )
    return {"added": len(items)}


def add_images(items: list) -> dict:
    if not items:
        return {"added": 0}
    vecs = embed.images([it["image_path"] for it in items])
    _col("image").upsert(
        ids=[it["id"] for it in items],
        embeddings=vecs,
        documents=[it["image_path"] for it in items],
        metadatas=[_clean_meta(it.get("metadata")) for it in items],
    )
    return {"added": len(items)}


def _query(col, vec: list, k: int, where) -> list:
    if col.count() == 0:
        return []
    res = col.query(query_embeddings=[vec], n_results=max(1, min(k, col.count())), where=where or None,
                    include=["documents", "metadatas", "distances"])
    return _hits(res["ids"][0], res["documents"][0], res["metadatas"][0], res["distances"][0])


def query_texts(query: str = "", k: int = 8, where=None, space: str = "", embedding: list = None) -> dict:
    col = _col("text", space)
    if col.count() == 0:
        return {"hits": []}  # don't load the embedding model for an empty collection
    if space and embedding is None:
        raise ValueError("API embedding space needs a precomputed query embedding")
    vec = embedding if embedding is not None else embed.query(query)
    return {"hits": _query(col, vec, k, where)}


def query_images(query: str, k: int = 2, where=None) -> dict:
    col = _col("image")
    if col.count() == 0:
        return {"hits": []}  # don't load CLIP when there are no pages
    return {"hits": _query(col, embed.image_query(query), k, where)}


def get_texts(where=None, limit: int = 50, space: str = "") -> dict:
    res = _col("text", space).get(where=where or None, limit=limit, include=["documents", "metadatas"])
    return {"hits": _hits(res["ids"], res["documents"], res["metadatas"])}


def delete(where=None, space: str = "", only_space: bool = False) -> dict:
    """Delete matching documents from every collection, or only from the text
    collection of `space` when only_space is set."""
    if not where:
        raise ValueError("refusing to delete without a filter")
    if only_space:
        _col("text", space).delete(where=where)
        return {"ok": True}
    db = _db()
    for c in db.list_collections():
        name = c if isinstance(c, str) else c.name
        db.get_collection(name, embedding_function=None).delete(where=where)
    return {"ok": True}
