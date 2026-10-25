"""Unit tests for the pure-python category decision rules in ai/main.py.

ai/main.py imports fastembed/fastapi/spacy/sklearn at module level, so it cannot
be imported on a bare Python runner. The rules live in a marked, dependency-free
block (`# --- CATEGORY RULE START/END`); this test extracts and executes exactly
that block, so it always tests the shipped code.

Run: python -m unittest discover -s ai -p 'test_*.py'
"""

import os
import re
import unittest

_SRC_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "main.py")
_MARKER = re.compile(r"# --- CATEGORY RULE START.*?# --- CATEGORY RULE END", re.S)


def _load_rules():
    src = open(_SRC_PATH, encoding="utf-8").read()
    match = _MARKER.search(src)
    if not match:
        raise AssertionError("category rule markers not found in ai/main.py")
    namespace = {"re": re}
    exec(match.group(0), namespace)  # noqa: S102 - executing our own source
    return namespace


RULES = _load_rules()
choose_categories = RULES["choose_categories"]
has_weather_evidence = RULES["has_weather_evidence"]
UNCLASSIFIED = RULES["UNCLASSIFIED"]
WEATHER = RULES["WEATHER_CATEGORY"]

CATS = ["Ελλάδα", "Κοινωνία", "Καιρός", UNCLASSIFIED]


class WeatherGateTest(unittest.TestCase):
    def test_headline_alone_is_enough(self):
        for title in [
            "Καταιγίδα Austen: Στο σκοτάδι 10.000 νοικοκυριά στη Γερμανία",
            "Πορτοκαλί προειδοποίηση για ισχυρές καταιγίδες στη Μαγνησία",
            "Έκτακτο δελτίο επιδείνωσης για 48ωρη κακοκαιρία",
            "Storm Austen leaves 82,000 homes without power",
        ]:
            self.assertTrue(has_weather_evidence(title, title), title)

    def test_body_needs_two_hits(self):
        self.assertTrue(has_weather_evidence("ισχυρές βροχές και καταιγίδες στη Μαγνησία"))
        self.assertFalse(has_weather_evidence("Βουτιές στην Αίγινα: ο καιρός είναι ακόμα ζεστός"))

    def test_unrelated_stories_fail(self):
        for text in [
            "Γκράβα: Έλεγχοι χωρίς διαρροή μεθανίου και προετοιμασία επαναλειτουργίας των σχολείων",
            "Καίρια ζητήματα για την κυβέρνηση",          # καιρία != καιρός
            "Πρόγνωση γιατρού: καλή πορεία της ασθενούς",  # medical prognosis
            "Πανσέληνος Οκτωβρίου: Πότε θα δούμε το Φεγγάρι του Κυνηγού",
            "Φυσιολογικό χρώμα ούρου και επίδραση της διατροφής",
            "Πώς θα αλλάξουν τα δημοτικά τέλη με το «Πληρώνω όσο πετάω»",
            "Carrick denies Manchester United are 'unmanageable'",
        ]:
            self.assertFalse(has_weather_evidence(text, text), text)

    def test_passing_mention_in_body_is_not_weather(self):
        """A celebrity piece that says "ο καιρός είναι ζεστός" once is not news about weather."""
        body = "Η Κλέλια Ανδριολάτου κάνει βουτιές στην Αίγινα· ο καιρός είναι ακόμα ζεστός."
        self.assertFalse(has_weather_evidence(body, "Κλέλια Ανδριολάτου: Βουτιές στην Αίγινα"))

    def test_empty(self):
        self.assertFalse(has_weather_evidence(""))
        self.assertFalse(has_weather_evidence(None))
        self.assertFalse(has_weather_evidence("", ""))


class ChooseCategoriesTest(unittest.TestCase):
    def _sims(self, mapping):
        return [mapping.get(c, 0.0) for c in CATS]

    def test_weather_masked_without_evidence(self):
        """Γκράβα case: Καιρός inside the old 0.07 band must not be tagged."""
        sims = self._sims({"Ελλάδα": 0.379, "Κοινωνία": 0.336, WEATHER: 0.328})
        self.assertEqual(choose_categories(CATS, sims, "μεθάνιο στα σχολεία"), ["Ελλάδα"])

    def test_weather_kept_with_evidence(self):
        sims = self._sims({WEATHER: 0.40, "Ελλάδα": 0.30})
        self.assertEqual(
            choose_categories(CATS, sims, "ισχυρές βροχές και καταιγίδες"), [WEATHER]
        )

    def test_below_threshold_is_unclassified(self):
        sims = self._sims({"Ελλάδα": 0.14, WEATHER: 0.13})
        self.assertEqual(choose_categories(CATS, sims, "κάτι"), [UNCLASSIFIED])

    def test_extra_category_needs_tight_delta_and_floor(self):
        sims = self._sims({"Ελλάδα": 0.40, "Κοινωνία": 0.35})
        self.assertEqual(choose_categories(CATS, sims, "σχολεία"), ["Ελλάδα"])

    def test_near_tie_keeps_second_category(self):
        sims = self._sims({"Ελλάδα": 0.40, "Κοινωνία": 0.395})
        self.assertEqual(choose_categories(CATS, sims, "σχολεία"), ["Ελλάδα", "Κοινωνία"])

    def test_unclassified_never_rides_along(self):
        sims = self._sims({"Ελλάδα": 0.40, UNCLASSIFIED: 0.395})
        self.assertEqual(choose_categories(CATS, sims, "σχολεία"), ["Ελλάδα"])

    def test_max_three_categories(self):
        sims = self._sims({"Ελλάδα": 0.40, "Κοινωνία": 0.395, WEATHER: 0.39})
        out = choose_categories(CATS, sims, "ισχυρές βροχές και καταιγίδες· σχολεία κλειστά")
        self.assertLessEqual(len(out), 3)
        self.assertIn(WEATHER, out)


if __name__ == "__main__":
    unittest.main()
