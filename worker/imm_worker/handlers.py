"""RPC method table. Heavy modules are imported on first use so `ping` stays instant."""


def _ping() -> dict:
    return {"ok": True}


def _extract(**kw):
    from . import docs
    return docs.extract(**kw)


def _vec(name):
    def call(**kw):
        from . import vectors
        return getattr(vectors, name)(**kw)
    return call


METHODS = {
    "ping": _ping,
    "doc.extract": _extract,
    "store.add_texts": _vec("add_texts"),
    "store.add_images": _vec("add_images"),
    "store.query_texts": _vec("query_texts"),
    "store.query_images": _vec("query_images"),
    "store.get_texts": _vec("get_texts"),
    "store.delete": _vec("delete"),
}
