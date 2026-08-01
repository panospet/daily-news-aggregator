import os
import requests

AI_SERVICE_URL = "http://127.0.0.1:8001/cluster"

def run_test(test_name, articles, expected_group_sizes):
    payload = {"articles": articles}
    try:
        response = requests.post(AI_SERVICE_URL, json=payload, timeout=30)
        response.raise_for_status()
        data = response.json()
    except Exception as e:
        print(f"[{test_name}] FAILED - API Request Error: {e}")
        return False

    # Extract all group sizes from the response
    actual_group_sizes = []
    for cat in data.get("categories", []):
        for group in cat.get("groups", []):
            actual_group_sizes.append(group["count"])
    
    # Sort for comparison
    actual_group_sizes.sort(reverse=True)
    expected_group_sizes.sort(reverse=True)

    if actual_group_sizes == expected_group_sizes:
        print(f"[{test_name}] PASSED")
        return True
    else:
        print(f"[{test_name}] FAILED")
        print(f"  Expected group sizes: {expected_group_sizes}")
        print(f"  Actual group sizes:   {actual_group_sizes}")
        # Print the topics to help debug
        for cat in data.get("categories", []):
            for group in cat.get("groups", []):
                titles = [a["title"] for a in group["articles"]]
                print(f"  -> Group ({group['count']}): {titles}")
        return False

def test_highly_similar_greek():
    """Test that 3 Greek articles about the exact same event are grouped together."""
    articles = [
        {"title": "Τραγωδία στην Αθηνών-Λαμίας", "content": "Σύγκρουση δύο φορτηγών με έναν νεκρό συνέβη σήμερα τα ξημερώματα.", "category": "news"},
        {"title": "Τροχαίο δυστύχημα προς Λαμία", "content": "Ένας νεκρός μετά από σύγκρουση δύο φορτηγών στην Αθηνών-Λαμίας.", "category": "news"},
        {"title": "Αθηνών - Λαμίας: Σύγκρουση", "content": "Απολογισμός ένα νεκρό μετά από καραμπόλα φορτηγών στην εθνική οδό.", "category": "news"},
    ]
    # Expected: 1 group of 3 articles (in Ελληνική ειδησεογραφία)
    return run_test("test_highly_similar_greek", articles, [3])

def test_unrelated_greek():
    """Test that 3 completely unrelated Greek articles are kept separate."""
    articles = [
        {"title": "Τραγωδία στην Αθηνών-Λαμίας", "content": "Ένας νεκρός από τροχαίο.", "category": "news"},
        {"title": "Ιγκουάνα 44 εκατομμυρίων χρόνων", "content": "Ταξίδεψε στην Ευρώπη με σχεδία σύμφωνα με τους επιστήμονες.", "category": "news"},
        {"title": "Συνταγή για παστίτσιο", "content": "Το πιο νόστιμο παραδοσιακό φαγητό για το κυριακάτικο τραπέζι.", "category": "news"},
    ]
    # Expected: 3 groups of 1 article (in Ελληνική ειδησεογραφία)
    return run_test("test_unrelated_greek", articles, [1, 1, 1])

def test_different_super_categories():
    """Test that articles with different super categories (sports vs news) are not grouped."""
    articles = [
        {"title": "Ολυμπιακοί Αγώνες", "content": "Η Εθνική Ελλάδος προκρίθηκε!", "category": "sports"},
        {"title": "Ολυμπιακοί Αγώνες", "content": "Η Εθνική Ελλάδος προκρίθηκε!", "category": "news"},
    ]
    # Expected: 2 groups of 1 article (one in Αθλητικές, one in Ελληνική)
    return run_test("test_different_super_categories", articles, [1, 1])

def test_language_separation():
    """Test that English and Greek articles are now separated into different buckets."""
    articles = [
        {"title": "Olympic Games", "content": "USA wins gold in basketball against France.", "category": "international"},
        {"title": "ΗΠΑ: Κατέκτησαν το χρυσό", "content": "Νίκησαν τη Γαλλία στο μπάσκετ.", "category": "international"},
    ]
    # Under old rules they clustered. Now one goes to Ξένη and one to Ελληνική!
    # Expected: 2 groups of 1
    return run_test("test_language_separation", articles, [1, 1])

def test_music_releases():
    """Test that music articles with release keywords go to a different bucket than normal music news."""
    articles = [
        {"title": "Η Taylor Swift", "content": "Κυκλοφόρησε το νέο της άλμπουμ σήμερα το πρωί.", "category": "music"},
        {"title": "Νέο τραγούδι", "content": "Από τους Coldplay, διαθέσιμο παντού.", "category": "music"},
        {"title": "Συναυλία των Coldplay", "content": "Στην Αθήνα το καλοκαίρι.", "category": "music"},
    ]
    # First two have keywords -> Νέες μουσικές κυκλοφορίες.
    # The smart mpnet model clusters them together because they are semantically similar!
    # Third has no keywords -> Μουσικά νέα.
    # Expected: 2 groups (1 group of 2 in new releases, 1 group of 1 in music news)
    return run_test("test_music_releases", articles, [2, 1])

def test_content_similarity():
    """Test that different titles with very similar content cluster correctly."""
    articles = [
        {"title": "Ανακοίνωση από το Υπουργείο", "content": "Αυξάνονται οι μισθοί στο δημόσιο από 1η Ιανουαρίου.", "category": "news"},
        {"title": "Καλά νέα για τους εργαζομένους", "content": "Αυξάνονται οι μισθοί στο δημόσιο από 1η Ιανουαρίου.", "category": "news"},
    ]
    # Expected: 1 group of 2
    return run_test("test_content_similarity", articles, [2])

if __name__ == "__main__":
    print("Running Clustering Test Cases...\n")
    results = [
        test_highly_similar_greek(),
        test_unrelated_greek(),
        test_different_super_categories(),
        test_language_separation(),
        test_music_releases(),
        test_content_similarity()
    ]
    
    if all(results):
        print("\nALL TESTS PASSED!")
    else:
        print("\nSOME TESTS FAILED!")
        exit(1)
