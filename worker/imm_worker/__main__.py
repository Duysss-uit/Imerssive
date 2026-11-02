"""JSON-RPC loop. One request per line on stdin, one response per line on stdout.

Request:  {"id": 1, "method": "store.query_texts", "params": {...}}
Response: {"id": 1, "result": {...}}  or  {"id": 1, "error": "message"}
"""
import json
import logging
import os
import sys
import threading
import traceback
from concurrent.futures import ThreadPoolExecutor

# Move the RPC pipes off fds 0/1. Anything a library prints goes to stderr, and
# nothing else touches the stdin pipe: on Windows, any call on a synchronous pipe
# handle (fstat, isatty...) blocks while our reader thread waits on it.
_rpc_out = os.fdopen(os.dup(sys.stdout.fileno()), "w", encoding="utf-8", buffering=1)
_rpc_in = os.fdopen(os.dup(sys.stdin.fileno()), "r", encoding="utf-8")
os.dup2(sys.stderr.fileno(), sys.stdout.fileno())
_null = os.open(os.devnull, os.O_RDONLY)
os.dup2(_null, sys.stdin.fileno())
sys.stdin = open(os.devnull, "r")

logging.basicConfig(stream=sys.stderr, level=logging.INFO, format="%(levelname)s %(name)s: %(message)s")
log = logging.getLogger("imm_worker")

from . import handlers  # noqa: E402  (import after stdout redirect)

_write_lock = threading.Lock()


def _reply(msg: dict) -> None:
    line = json.dumps(msg, ensure_ascii=False)
    with _write_lock:
        _rpc_out.write(line + "\n")
        _rpc_out.flush()


def _handle(req: dict) -> None:
    rid = req.get("id")
    method = req.get("method", "")
    try:
        fn = handlers.METHODS.get(method)
        if fn is None:
            raise ValueError(f"unknown method {method!r}")
        result = fn(**(req.get("params") or {}))
        _reply({"id": rid, "result": result})
    except Exception as e:  # report every failure to the caller instead of dying
        log.error("%s failed: %s\n%s", method, e, traceback.format_exc())
        _reply({"id": rid, "error": f"{type(e).__name__}: {e}"})


def main() -> None:
    log.info("worker ready (pid %d)", os.getpid())
    with ThreadPoolExecutor(max_workers=int(os.environ.get("IMM_THREADS", "2"))) as pool:
        for line in _rpc_in:
            line = line.strip()
            if not line:
                continue
            try:
                req = json.loads(line)
            except json.JSONDecodeError:
                log.error("bad request line: %s", line[:200])
                continue
            pool.submit(_handle, req)
        # stdin closed: finish queued work, then exit (pool context waits)
    log.info("stdin closed, exiting")


if __name__ == "__main__":
    main()
