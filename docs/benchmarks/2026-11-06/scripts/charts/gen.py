"""Generate one benchmark chart with the OpenAI image API (image-only key).

Usage: python3 gen.py <prompt_file> <out_png> [model] [size]
The key is read from ~/.secrets/openai-image-only.key and never printed.
"""
import base64
import json
import logging
import sys
import urllib.error
import urllib.request
from pathlib import Path

logging.basicConfig(level=logging.INFO, format="%(message)s")
log = logging.getLogger("gen")

KEY_FILE = Path.home() / ".secrets" / "openai-image-only.key"


def load_key() -> str:
    """Return the key value from the KEY=value secrets file."""
    _, _, val = KEY_FILE.read_text().strip().partition("=")
    return val.strip().strip("\"'")


def generate(prompt: str, out: Path, model: str, size: str) -> int:
    """Call images/generations and write the PNG; return an exit code."""
    body = json.dumps(
        {"model": model, "prompt": prompt, "size": size, "quality": "high", "n": 1}
    ).encode()
    req = urllib.request.Request(
        "https://api.openai.com/v1/images/generations",
        data=body,
        headers={"Authorization": f"Bearer {load_key()}", "Content-Type": "application/json"},
    )
    try:
        data = json.load(urllib.request.urlopen(req, timeout=600))
    except urllib.error.HTTPError as exc:
        log.error("http %s: %s", exc.code, exc.read().decode()[:600])
        return 1
    out.write_bytes(base64.b64decode(data["data"][0]["b64_json"]))
    log.info("wrote %s usage=%s", out, json.dumps(data.get("usage", {})))
    return 0


def main() -> int:
    """Parse argv and run."""
    prompt = Path(sys.argv[1]).read_text()
    out = Path(sys.argv[2])
    model = sys.argv[3] if len(sys.argv) > 3 else "gpt-image-2.6-sunburst"
    size = sys.argv[4] if len(sys.argv) > 4 else "2048x1152"
    return generate(prompt, out, model, size)


if __name__ == "__main__":
    sys.exit(main())
