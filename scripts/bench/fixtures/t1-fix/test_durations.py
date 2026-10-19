import unittest

from durations import parse_duration


class TestParseDuration(unittest.TestCase):
    def test_hours(self):
        self.assertEqual(parse_duration("2h"), 7200)

    def test_minutes(self):
        self.assertEqual(parse_duration("1h30m"), 5400)


if __name__ == "__main__":
    unittest.main()
