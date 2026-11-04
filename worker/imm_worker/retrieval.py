"""Hybrid retrieval with LangChain: dense vectors (Chroma) and BM25 keywords,
fused by EnsembleRetriever (weighted reciprocal rank fusion).

Dense search finds passages by meaning; BM25 finds exact names and rare words
that a small multilingual embedding model tends to blur. Each embedding space
(the local model, or an API model whose query vector the app computes and
passes in) gets a dense retriever and a BM25 retriever over the same texts.

LangChain is imported on the first search, so the worker starts fast."""
import json
import re
import threading

from . import embed, vectors

DENSE_WEIGHT = 0.6
KEYWORD_WEIGHT = 0.4

_lock = threading.Lock()
_bm25_cache: dict = {}  # (collection, where) -> (version, retriever)
_lc = None


def _langchain():
    """Import LangChain and define the small adapters it needs, once."""
    global _lc
    with _lock:
        if _lc is not None:
            return _lc
        from langchain_chroma import Chroma
        from langchain_classic.retrievers import EnsembleRetriever
        from langchain_core.documents import Document
        from langchain_core.embeddings import Embeddings
        from langchain_core.retrievers import BaseRetriever
        from rank_bm25 import BM25Okapi

        class LocalEmbeddings(Embeddings):
            """The worker's fastembed model."""
            def embed_documents(self, texts):
                return embed.passages(texts)

            def embed_query(self, text):
                return embed.query(text)

        class GivenQuery(Embeddings):
            """An API model's query vector, computed by the app."""
            def __init__(self, vector):
                self.vector = vector

            def embed_documents(self, texts):
                raise RuntimeError("documents of an API space are embedded by the app")

            def embed_query(self, text):
                return self.vector

        class BM25(BaseRetriever):
            """Okapi BM25 over a fixed set of documents."""
            docs: list
            index: object
            k: int = 8

            @classmethod
            def build(cls, docs, k):
                return cls(docs=docs, index=BM25Okapi([_tokens(d.page_content) for d in docs]), k=k)

            def _get_relevant_documents(self, query, *, run_manager=None):
                scores = self.index.get_scores(_tokens(query))
                best = sorted(range(len(scores)), key=lambda i: scores[i], reverse=True)[: self.k]
                return [self.docs[i] for i in best if scores[i] > 0]

        _lc = dict(Chroma=Chroma, EnsembleRetriever=EnsembleRetriever, Document=Document,
                   LocalEmbeddings=LocalEmbeddings, GivenQuery=GivenQuery, BM25=BM25)
        return _lc


def _tokens(text: str) -> list:
    # \w is Unicode-aware: Vietnamese syllables, Japanese runs and names all count.
    return re.findall(r"\w+", text.lower())


def _bm25(col, where, k):
    """A BM25 retriever over the collection's texts matching where, rebuilt
    only when the store changed since it was made."""
    lc = _langchain()
    key = (col.name, json.dumps(where, sort_keys=True))
    with _lock:
        cached = _bm25_cache.get(key)
    if cached and cached[0] == vectors.version():
        retriever = cached[1]
        retriever.k = k
        return retriever
    res = col.get(where=where or None, include=["documents", "metadatas"])
    docs = [lc["Document"](id=i, page_content=t or "", metadata=m or {})
            for i, t, m in zip(res["ids"], res["documents"], res["metadatas"])]
    if not docs:
        return None
    retriever = lc["BM25"].build(docs, k)
    with _lock:
        _bm25_cache[key] = (vectors.version(), retriever)
    return retriever


def search(query: str, k: int = 8, spaces: list = None) -> dict:
    """spaces: [{"name": "" for the local model or an API space, "where": filter,
    "embedding": query vector for an API space}]. Returns fused hits, best first."""
    lc = _langchain()
    retrievers, weights, errors = [], [], []
    for sp in spaces or [{"name": ""}]:
        name, where = sp.get("name", ""), sp.get("where")
        try:
            col = vectors._col("text", name)
            if col.count() == 0:
                continue
            if name and sp.get("embedding") is None:
                raise ValueError("API embedding space needs a precomputed query embedding")
            emb = lc["GivenQuery"](sp["embedding"]) if name else lc["LocalEmbeddings"]()
            store = lc["Chroma"](client=vectors._db(), collection_name=col.name, embedding_function=emb)
            retrievers.append(store.as_retriever(search_kwargs={"k": k, "filter": where or None}))
            weights.append(DENSE_WEIGHT)
            bm25 = _bm25(col, where, k)
            if bm25 is not None:
                retrievers.append(bm25)
                weights.append(KEYWORD_WEIGHT)
        except Exception as e:  # one broken space shouldn't blind the rest
            errors.append(f"{name or 'local'}: {e}")
    if not retrievers:
        if errors:
            raise RuntimeError("; ".join(errors))
        return {"hits": []}
    docs = lc["EnsembleRetriever"](retrievers=retrievers, weights=weights).invoke(query)[:k]
    # Fusion ranks rather than scores: report the rank as a distance in [0, 1).
    hits = [{"id": d.id or "", "text": d.page_content, "metadata": d.metadata or {}, "distance": i / max(len(docs), 1)}
            for i, d in enumerate(docs)]
    return {"hits": hits, "errors": errors}
