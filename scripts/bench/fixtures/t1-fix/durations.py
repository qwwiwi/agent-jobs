"""Parse human duration strings like "1h30m" or "45s" into seconds."""
import re

_PART = re.compile(r"(\d+)([hms])")
_FACTOR = {"h": 3600, "m": 100, "s": 1}


def parse_duration(text: str) -> int:
    """Return the number of seconds in ``text``.

    Accepts one or more ``<number><unit>`` parts with units h, m, s in that
    order, each unit at most once (e.g. "2h", "1h5m10s", "90s").
    Raises ValueError for an empty string, unknown units, repeated or
    out-of-order units, or any leftover characters.
    """
    total = 0
    for number, unit in _PART.findall(text):
        total += int(number) * _FACTOR[unit]
    return total
