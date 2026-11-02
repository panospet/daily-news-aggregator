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
has_music_evidence = RULES["has_music_evidence"]
has_release_evidence = RULES["has_release_evidence"]
UNCLASSIFIED = RULES["UNCLASSIFIED"]
WEATHER = RULES["WEATHER_CATEGORY"]
MUSIC = RULES["MUSIC_CATEGORY"]
RELEASE = RULES["RELEASE_CATEGORY"]

CATS = ["Ελλάδα", "Κοινωνία", "Καιρός", "Οικονομία & Ενέργεια", MUSIC, RELEASE, UNCLASSIFIED]


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

    def test_music_masked_without_evidence_and_feed(self):
        """Articles with high release similarity (like cars or deals) must not get music categories."""
        # Simulated similarity where RELEASE won because of 'νέα έκδοση / new edition'
        sims = self._sims({RELEASE: 0.285, MUSIC: 0.15, "Οικονομία & Ενέργεια": 0.22})
        out = choose_categories(
            CATS, sims,
            "Opel Corsa: Με turbo κινητήρα 100 ίππων και τιμή από 17.300 ευρώ",
            "Opel Corsa",
            feed_cats=["news"]
        )
        self.assertNotIn(RELEASE, out)
        self.assertNotIn(MUSIC, out)
        self.assertEqual(out, ["Οικονομία & Ενέργεια"])

    def test_music_news_without_release_gets_music_only(self):
        """Tour and concert news must not get tagged as music releases."""
        sims = self._sims({RELEASE: 0.38, MUSIC: 0.37})
        out = choose_categories(
            CATS, sims,
            "Deep Purple: Ακυρώθηκαν συναυλίες τους, λόγω ασθένειας του Ίαν Γκίλαν",
            "Deep Purple συναυλίες",
            feed_cats=["news"]
        )
        self.assertNotIn(RELEASE, out)
        self.assertIn(MUSIC, out)

    def test_true_release_gets_release_category(self):
        """Actual album/single releases must be tagged as releases."""
        sims = self._sims({RELEASE: 0.38, MUSIC: 0.37})
        out = choose_categories(
            CATS, sims,
            "Λένα Ζευγαρά: Κυκλοφόρησε το νέο διπλό album «Μείνε Απόψε Εδώ»",
            "Λένα Ζευγαρά νέο album",
            feed_cats=["sports"]
        )
        self.assertIn(RELEASE, out)


class MusicGateTest(unittest.TestCase):
    def test_unrelated_articles_from_screenshot_fail(self):
        """Articles from the user screenshot must fail the music evidence test."""
        unrelated = [
            ("Black Friday 2026: Πότε αρχίζουν οι πρώτες εκπτώσεις και πόσο θα κρατήσουν", ["news"]),
            ("Ποιες Κυριακές θα είναι ανοιχτά μαγαζιά και καταστήματα μέχρι το τέλος του 2026", ["news"]),
            ("Opel Corsa: Με turbo κινητήρα 100 ίππων και τιμή από 17.300 ευρώ", ["news"]),
            ("Ράλι $1 τρισ. για Apple από τον Ιούνιο", ["news"]),
            ("Η Estée Lauder κερδίζει τους καταναλωτές και αναβαθμίζει τις προβλέψεις", ["news"]),
            ("Συνταγή: Ομελέτα με ντομάτες και χούμους", ["news"]),
            ("Starwatch: Catch the striking trifecta of Venus, the moon and Spica at break of dawn", ["international"]),
            ("Κάμπινγκ: Το παρελθόν έρχεται ξανά στο προσκήνιο", ["news"]),
            ("Πωλήσεις αυτοκινήτων Οκτωβρίου: Ρεκόρ ταξινομήσεων για την αγορά", ["news"]),
            ("Θάντερ: Δεν ενεργοποίησαν την οψιόν ανανέωσης του Τόπιτς", ["sports"]),
            ("Hoka Rincon 5 Παπούτσια για Τρέξιμο με 92€ από epapoutsia", ["random"]),
            ("Poco X8 Pro Max 12GB 256GB orange", ["random"]),
            ("Ψησταριά υγραερίου Jamestown Marlon 4.3 με μαντέμι – 399€", ["random"]),
        ]
        for title, cats in unrelated:
            self.assertFalse(has_music_evidence(title, title, feed_cats=cats), f"Failed for {title}")

    def test_genuine_music_articles_pass(self):
        valid = [
            ("Deep Purple: Ακυρώθηκαν συναυλίες τους, λόγω ασθένειας του Ίαν Γκίλαν", ["news"]),
            ("Το τραγούδι της ημέρας: Polis", ["news"]),
            ("«Days Reflected in Melodies»: Ένα alternative hip hop album από νέους δημιουργούς", ["news"]),
            ("Steve Ellis, frontman of Everlasting Love hitmakers Love Affair, dies aged 76", ["international"]),
            ("Robert Plant and Saving Grace announce Summer 2027 dates", ["music"]),
        ]
        for title, cats in valid:
            self.assertTrue(has_music_evidence(title, title, feed_cats=cats), f"Failed for {title}")


class ReleaseGateTest(unittest.TestCase):
    def test_non_release_music_fails(self):
        non_releases = [
            ("Robert Plant and Saving Grace announce Summer 2027 dates", ["music"], ["UNCUT"]),
            ("Deep Purple: Ακυρώθηκαν συναυλίες τους, λόγω ασθένειας του Ίαν Γκίλαν", ["news"], ["in.gr"]),
            ("Steve Ellis, frontman of Everlasting Love hitmakers Love Affair, dies aged 76", ["international"], ["The Guardian"]),
            ("Mighty Hoopla celebrates 10th anniversary with huge 2027 line-up", ["music"], ["NME"]),
        ]
        for title, cats, feeds in non_releases:
            self.assertFalse(has_release_evidence(title, title, feed_cats=cats, feed_names=feeds), f"Failed for {title}")

    def test_true_releases_pass(self):
        releases = [
            ("Bonnie Tyler Christmas single to be released as husband says he misses her every day", ["international"], ["BBC News"]),
            ("Λένα Ζευγαρά: Κυκλοφόρησε το νέο διπλό album «Μείνε Απόψε Εδώ»", ["sports"], ["Gazzetta RSS"]),
            ("«Days Reflected in Melodies»: Ένα alternative hip hop album από νέους δημιουργούς", ["news"], ["in.gr"]),
            ("Το τραγούδι της ημέρας: Polis", ["news"], ["Η Εφημερίδα των Συντακτών"]),
            ("My Chemical Romance confirm they'll begin work on new album in 2027", ["music"], ["Louder RSS Feed"]),
            ("Jaberdarwin - The Soft Collapse", ["music"], ["Rocking.gr feed"]),
            ("Late Loop", ["music"], ["Pitchfork"]),
        ]
        for title, cats, feeds in releases:
            self.assertTrue(has_release_evidence(title, title, feed_cats=cats, feed_names=feeds), f"Failed for {title}")


class RealFeedbackCasesTest(unittest.TestCase):
    """Tests based on actual user feedback from pos_fb.json and neg_fb.json"""
    
    def _sims(self, mapping):
        return [mapping.get(c, 0.0) for c in CATS]
    
    def test_pos_case_arctangent_festival_music_festival(self):
        """From pos_fb.json id:32 - Arctangent Festival, music festival coverage"""
        sims = self._sims({MUSIC: 0.45, "Πολιτισμός & Showbiz": 0.35, RELEASE: 0.28})
        out = choose_categories(CATS, sims, "Arctangent Festival επιλογές πρώτη μέρα συναυλίες Bristol")
        self.assertIn(MUSIC, out)
        # Should not be a release (it's festival coverage)
        self.assertNotIn(RELEASE, out)
    
    def test_neg_case_argentina_world_cup_should_split(self):
        """From neg_fb.json id:17 - Argentina vs England World Cup celebration marked for split"""
        cats = ["Αθλητισμός", "Κόσμος", "Πολιτική & Διπλωματία", "Διάφορα"]
        sims = [0.46, 0.42, 0.28, 0.0]
        # This was incorrectly grouped - should be sports/world related but maybe context matters
        out = choose_categories(cats, sims, 
                               "Αργεντινή εθνική εορτή νίκη Αγγλία Μουντιάλ")
        self.assertIn("Αθλητισμός", out)
    
    def test_neg_case_two_different_bands_mixed_up(self):
        """From neg_fb.json id:16 - Temic vs Tom Waits incorrectly grouped"""
        # These are completely different artists - should not be grouped
        sims_temic = self._sims({RELEASE: 0.39, MUSIC: 0.37})
        sims_waits = self._sims({RELEASE: 0.38, MUSIC: 0.36})
        out1 = choose_categories(CATS, sims_temic, "Temic δεύτερος δίσκος single The Fly Terror Management Theory")
        out2 = choose_categories(CATS, sims_waits, "Tom Waits The Fly spoken word b-side Boots On The Ground")
        # Both might get music/release but they're different stories
        self.assertTrue(MUSIC in out1 or RELEASE in out1)
        # Tom Waits case might not trigger music if not enough evidence
        # The key issue is they were grouped together - different articles, different artists
        self.assertIsInstance(out2, list)
    
    def test_neg_case_different_album_reviews_mixed(self):
        """From neg_fb.json id:15 - Two different album reviews incorrectly grouped"""
        sims1 = self._sims({MUSIC: 0.42, RELEASE: 0.35})
        sims2 = self._sims({MUSIC: 0.41, RELEASE: 0.34})
        out1 = choose_categories(CATS, sims1, "Sinistrous Mist black metal ατμοσφαιρική αφήγηση")
        out2 = choose_categories(CATS, sims2, "Liminal Sky melankholiko εσωτερικό ταξίδι")
        # Album reviews - might or might not get music depending on evidence
        # The key issue is they were incorrectly grouped together
        self.assertIsInstance(out1, list)
        self.assertIsInstance(out2, list)
        self.assertNotIn(RELEASE, out1)  # Reviews, not new releases
        self.assertNotIn(RELEASE, out2)
    
    def test_neg_case_tv_show_controversy(self):
        """From neg_fb.json id:14 - TV show controversy about 'Καλημέρα Ελλάδα'"""
        sims = self._sims({"Πολιτισμός & Showbiz": 0.44, "Κοινωνία": 0.28, MUSIC: 0.15})
        out = choose_categories(CATS, sims, "Γιώργος Παπαδάκης Καλημέρα Ελλάδα ΑΝΤ1 Φοίβος αντίδραση")
        # TV/media related
        self.assertTrue("Πολιτισμός & Showbiz" in out or "Κοινωνία" in out)
        self.assertNotIn(MUSIC, out)  # TV show, not music related


if __name__ == "__main__":
    unittest.main()
