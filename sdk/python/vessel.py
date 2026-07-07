"""vessel Python SDK — minimal client for the vessel sandbox API.

Zero dependencies (stdlib urllib only).

    from vessel import VesselClient

    v = VesselClient("http://localhost:7070")
    sb = v.create(driver="process")
    result = sb.exec(["python3", "-c", "print(21*2)"])
    print(result.stdout)          # "42\n"
    clone = sb.fork("/var/lib/vessel/snap-1")   # VM drivers only
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from dataclasses import dataclass, field


class VesselError(RuntimeError):
    pass


@dataclass
class ExecResult:
    exit_code: int
    stdout: str
    stderr: str


@dataclass
class Sandbox:
    id: str
    state: str
    _client: "VesselClient" = field(repr=False)

    def exec(self, cmd: list[str]) -> ExecResult:
        out = self._client._post(f"/v1/sandboxes/{self.id}/exec", {"cmd": cmd})
        return ExecResult(out["exit_code"], out["stdout"], out["stderr"])

    def snapshot(self, path: str) -> None:
        self._client._post(f"/v1/sandboxes/{self.id}/snapshot", {"path": path})

    def fork(self, path: str) -> "Sandbox":
        out = self._client._post(f"/v1/sandboxes/{self.id}/fork", {"path": path})
        return Sandbox(out["id"], out["state"], self._client)


class VesselClient:
    def __init__(self, base_url: str = "http://localhost:7070", timeout: float = 30.0):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout

    def create(self, driver: str = "process", spec: dict | None = None) -> Sandbox:
        out = self._post("/v1/sandboxes", {"driver": driver, "spec": spec or {}})
        return Sandbox(out["id"], out["state"], self)

    def list(self) -> list[Sandbox]:
        out = self._get("/v1/sandboxes")
        return [Sandbox(x["id"], x["state"], self) for x in out]

    def healthy(self) -> bool:
        try:
            return self._raw("GET", "/healthz") == b"ok"
        except VesselError:
            return False

    # -- internals --------------------------------------------------------

    def _get(self, path: str):
        return json.loads(self._raw("GET", path))

    def _post(self, path: str, body: dict):
        data = json.dumps(body).encode()
        return json.loads(self._raw("POST", path, data))

    def _raw(self, method: str, path: str, data: bytes | None = None) -> bytes:
        req = urllib.request.Request(
            self.base_url + path,
            data=data,
            method=method,
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                return resp.read()
        except urllib.error.HTTPError as e:
            raise VesselError(f"{method} {path}: HTTP {e.code}: {e.read().decode(errors='replace').strip()}") from e
        except urllib.error.URLError as e:
            raise VesselError(f"{method} {path}: {e.reason}") from e
