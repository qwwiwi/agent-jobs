import sys, unittest
sys.path.insert(0, sys.argv.pop(1))
from durations import parse_duration


class Hidden(unittest.TestCase):
    def test_values(self):
        for s, v in [("2h", 7200), ("1h30m", 5400), ("45s", 45), ("1h5m10s", 3910), ("90m", 5400), ("0s", 0)]:
            self.assertEqual(parse_duration(s), v, s)

    def test_rejects(self):
        for s in ["", "abc", "10", "5x", "1h1h", "30m1h", "1h 30m", "1h30mfoo", "-5m"]:
            with self.assertRaises(ValueError, msg=repr(s)):
                parse_duration(s)


unittest.main(argv=["x"])
