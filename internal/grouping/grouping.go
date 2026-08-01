package grouping

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/panospet/htmx-daily-news/internal/config"
	"github.com/panospet/htmx-daily-news/internal/gemini"
	"github.com/panospet/htmx-daily-news/internal/models"
)

// Service handles AI-powered article grouping.
type Service struct {
	Gemini gemini.Caller
}

// GroupArticles calls the Python AI service to cluster articles.
// Falls back to flat grouping if the service fails.
func (s *Service) GroupArticles(allArticles []models.Article) ([]models.AICategoryGroup, error) {
	tTotal := time.Now()

	reqBody, err := json.Marshal(map[string]interface{}{"articles": allArticles})
	if err != nil {
		return s.fallbackFlat(allArticles), nil
	}

	resp, err := http.Post("http://127.0.0.1:8001/cluster", "application/json", bytes.NewBuffer(reqBody))
	if err != nil || resp.StatusCode != 200 {
		log.Printf("[clustering] AI service call failed: %v", err)
		return s.fallbackFlat(allArticles), nil
	}
	defer resp.Body.Close()

	var data models.AIDashboardData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("[clustering] failed to unmarshal AI service response: %v", err)
		return s.fallbackFlat(allArticles), nil
	}

	log.Printf("[clustering] Remote AI clustering completed in %v", time.Since(tTotal))
	return data.Categories, nil
}

func (s *Service) fallbackFlat(allArticles []models.Article) []models.AICategoryGroup {
	catMap := make(map[string][]models.Article)
	for _, a := range allArticles {
		catMap[a.Category] = append(catMap[a.Category], a)
	}
	preferredOrder := []string{"international", "news", "sports", "music", "politics", "random", "panionara"}
	return FlatGroupResult(catMap, preferredOrder, config.CatDisplayName)
}

// GroupCategory groups articles within a single category using Gemini.
func (s *Service) GroupCategory(cat string, arts []models.Article, displayName string) (models.AICategoryGroup, error) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`Είσαι βοηθός ομαδοποίησης ειδήσεων για την κατηγορία "%s".

Ομαδοποίησε άρθρα που αναφέρονται στο ΙΔΙΟ συγκεκριμένο γεγονός ή εξέλιξη (π.χ. φωτιά στην ίδια περιοχή, ίδιο διπλωματικό επεισόδιο, ίδια σεισμική δόνηση, ίδιο φεστιβάλ/συναυλία).

Κανόνες:
- ΝΑΙ σε ομαδοποίηση: ίδιο φυσικό φαινόμενο/καταστροφή, ίδια επίθεση/σύγκρουση, ίδιο πολιτικό γεγονός, ίδια αθλητική διοργάνωση, ίδιο μουσικό event/άλμπουμ, ίδια δικαστική υπόθεση.
- ΟΧΙ σε γενικές κατηγορίες: μη βάζεις μαζί άρθρα που απλά έχουν ίδια λέξη-κλειδί αλλά διαφορετικό γεγονός.
- Αν 2+ άρθρα καλύπτουν το ίδιο θέμα, ομαδοποίησέ τα — ακόμα κι αν έχουν ελαφρώς διαφορετική οπτική.
- Ο τίτλος topic να είναι στα ΕΛΛΗΝΙΚΑ. Απαγορεύονται αγγλικοί τίτλοι.
- ΠΡΟΣΟΧΗ στα article_indices: τα indices ξεκινάνε από 0 για το πρώτο άρθρο.

Επιστρέψε ΜΟΝΟ JSON:
{"groups": [{"topic": "Ελληνικός τίτλος θέματος", "article_indices": [0, 3, 7], "sentence": "Νέα εξέλιξη..."}]}

Άρθρα (%d):
`, displayName, len(arts)))
	for i, a := range arts {
		sb.WriteString(fmt.Sprintf("[%d] %s — %s\n", i, a.Title, a.Feed))
	}

	prompt := sb.String()

	// For large categories, use gemini-2.5-flash (64K output tokens) for grouping
	jsonStr, err := s.Gemini.CallGrouping(prompt)
	if err != nil {
		return models.AICategoryGroup{}, err
	}

	var batchResult struct {
		Groups []struct {
			Topic          string `json:"topic"`
			ArticleIndices []int  `json:"article_indices"`
			Sentence       string `json:"sentence"`
		} `json:"groups"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &batchResult); err != nil {
		return models.AICategoryGroup{}, fmt.Errorf("parse groups: %w", err)
	}

	return BuildAICatGroupFromParsed(cat, arts, displayName, batchResult.Groups), nil
}

// GroupWithCategory processes articles and assigns each group to a display category.
func (s *Service) GroupWithCategory(cat string, arts []models.Article) (map[string][]models.AIGroup, error) {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(`Είσαι βοηθός ομαδοποίησης και κατηγοριοποίησης ειδήσεων για την αρχική κατηγορία "%s".

Ομαδοποίησε άρθρα που αναφέρονται στο ΙΔΙΟ συγκεκριμένο γεγονός ή εξέλιξη.

Στη συνέχεια, για κάθε ομάδα, επίλεξε σε ποια κατηγορία ανήκει:
- "ειδήσεις Κόσμος": για διεθνή γεγονότα (π.χ. πόλεμος ΗΠΑ-Ιράν, διεθνείς συμφωνίες, κλιματική κρίση)
- "ειδήσεις Ελλάδα": για γεγονότα που αφορούν την Ελλάδα (ελληνική πολιτική/οικονομία/κοινωνία, εγχώρια νέα)
- "μουσικά νέα": για νέα που αφορούν τη μουσική βιομηχανία, καλλιτέχνες, κυκλοφορίες άλμπουμ, συναυλίες, κριτικές μουσικής
- "αθλητικές ειδήσεις": για αθλητικά γεγονότα, αγώνες, διοργανώσεις (π.χ. Μουντιάλ, Ολυμπιακοί, ποδόσφαιρο, μπάσκετ κλπ)
- "διάφορες ειδήσεις": για επιστήμη, τεχνολογία, lifestyle, ή οτιδήποτε άλλο δεν ταιριάζει στα παραπάνω

ΠΡΟΣΟΧΗ: ΑΚΟΜΗ ΚΙ ΑΝ ΤΟ ΑΡΘΡΟ ΠΡΟΕΡΧΕΤΑΙ ΑΠΟ ΕΛΛΗΝΙΚΗ ΕΦΗΜΕΡΙΔΑ, αν το θέμα είναι διεθνές, η κατηγορία πρέπει να είναι "ειδήσεις Κόσμος". Αν ένα άρθρο από μουσικό feed/source αφορά αθλητικά, η κατηγορία πρέπει να είναι "αθλητικές ειδήσεις" ή "πολιτική" ("ειδήσεις Ελλάδα" ή "ειδήσεις Κόσμος").

Κανόνες:
- ΝΑΙ σε ομαδοποίηση: ίδιο φυσικό φαινόμενο/καταστροφή, ίδια σύγκρουση, ίδιο πολιτικό γεγονός, ίδια μουσική κυκλοφορία/κριτική, ίδιο αθλητικό γεγονός.
- ΟΧΙ σε γενικές κατηγορίες: μη βάζεις μαζί άρθρα με ίδια λέξη-κλειδί αλλά διαφορετικό γεγονός.
- Ο τίτλος topic στα ΕΛΛΗΝΙΚΑ.
- Το category: μία από τις display κατηγορίες που αναφέρθηκαν παραπάνω ("ειδήσεις Κόσμος", "ειδήσεις Ελλάδα", "μουσικά νέα", "αθλητικές ειδήσεις", "διάφορες ειδήσεις").
- article_indices ξεκινάνε από 0.

Επιστρέψε ΜΟΝΟ JSON:
{"groups": [{"topic": "...", "category": "μουσικά νέα", "article_indices": [0, 3], "sentence": "..."}]}

Άρθρα (%d):
`, config.CatDisplayName[cat], len(arts)))
	for i, a := range arts {
		sb.WriteString(fmt.Sprintf("[%d] %s — %s\n", i, a.Title, a.Feed))
	}

	prompt := sb.String()
	jsonStr, err := s.Gemini.CallGrouping(prompt)
	if err != nil {
		return nil, err
	}

	var batchResult struct {
		Groups []struct {
			Topic          string `json:"topic"`
			Category       string `json:"category"`
			ArticleIndices []int  `json:"article_indices"`
			Sentence       string `json:"sentence"`
		} `json:"groups"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &batchResult); err != nil {
		return nil, fmt.Errorf("parse with-category groups: %w", err)
	}

	// Distribute groups by assigned category
	result := map[string][]models.AIGroup{
		"ειδήσεις Κόσμος":      {},
		"ειδήσεις Ελλάδα":     {},
		"μουσικά νέα":          {},
		"αθλητικές ειδήσεις":   {},
		"διάφορες ειδήσεις":    {},
	}
	used := make(map[int]bool)

	for _, g := range batchResult.Groups {
		var grouped []models.Article
		for _, idx := range g.ArticleIndices {
			if idx >= 0 && idx < len(arts) && !used[idx] {
				grouped = append(grouped, arts[idx])
				used[idx] = true
			}
		}
		if len(grouped) == 0 {
			continue
		}
		ag := models.AIGroup{Topic: g.Topic, Articles: grouped, Count: len(grouped)}
		target := config.CatDisplayName[cat] // default to original display name
		if target == "" {
			target = "διάφορες ειδήσεις"
		}

		// Map generated category to valid keys
		catNorm := strings.TrimSpace(g.Category)
		switch {
		case strings.Contains(catNorm, "Κόσμος") || strings.Contains(catNorm, "World"):
			target = "ειδήσεις Κόσμος"
		case strings.Contains(catNorm, "Ελλάδα") || strings.Contains(catNorm, "Greece"):
			target = "ειδήσεις Ελλάδα"
		case strings.Contains(catNorm, "μουσικ") || strings.Contains(catNorm, "music") || strings.Contains(catNorm, "Music"):
			target = "μουσικά νέα"
		case strings.Contains(catNorm, "αθλητ") || strings.Contains(catNorm, "sport") || strings.Contains(catNorm, "Sport"):
			target = "αθλητικές ειδήσεις"
		case strings.Contains(catNorm, "διάφορ") || strings.Contains(catNorm, "random") || strings.Contains(catNorm, "misc") || strings.Contains(catNorm, "various"):
			target = "διάφορες ειδήσεις"
		}

		result[target] = append(result[target], ag)
	}

	// Ungrouped articles go to the original category display name
	for i, a := range arts {
		if !used[i] {
			dName := config.CatDisplayName[cat]
			if dName == "" {
				dName = "διάφορες ειδήσεις"
			}
			result[dName] = append(result[dName], models.AIGroup{
				Topic: a.Title, Articles: []models.Article{a}, Count: 1,
			})
		}
	}

	return result, nil
}

// GroupMergedCategory sends a combined batch of articles from news+politics+international
// to Gemini, which returns topic groups each with an assigned display category.
func (s *Service) GroupMergedCategory(arts []models.Article) (map[string]models.AICategoryGroup, error) {
	var sb strings.Builder
	sb.WriteString(`Είσαι βοηθός ομαδοποίησης και κατηγοριοποίησης ειδήσεων.

Ομαδοποίησε άρθρα που αναφέρονται στο ΙΔΙΟ συγκεκριμένο γεγονός ή εξέλιξη.

Στη συνέχεια, για κάθε ομάδα, επίλεξε σε ποια κατηγορία ανήκει:
- "ειδήσεις Κόσμος": για διεθνή γεγονότα (π.χ. πόλεμος ΗΠΑ-Ιράν, διεθνείς συμφωνίες, κλιματική κρίση, καταστροφές στο εξωτερικό)
- "ειδήσεις Ελλάδα": για γεγονότα που αφορούν την Ελλάδα (ελληνική πολιτική/οικονομία/κοινωνία, φωτιές στην Ελλάδα, ελληνικά δικαστήρια κλπ)

ΑΚΟΜΗ ΚΙ ΑΝ ΤΟ ΑΡΘΡΟ ΠΡΟΕΡΧΕΤΑΙ ΑΠΟ ΕΛΛΗΝΙΚΗ ΕΦΗΜΕΡΙΔΑ, αν το θέμα είναι διεθνές (π.χ. πόλεμος ΗΠΑ-Ιράν, γεγονότα σε άλλες χώρες, κλιματική σύνοδος), η κατηγορία πρέπει να είναι "ειδήσεις Κόσμος".

Κανόνες:
- ΝΑΙ σε ομαδοποίηση: ίδιο φυσικό φαινόμενο/καταστροφή, ίδια επίθεση/σύγκρουση, ίδιο πολιτικό γεγονός, ίδιο αθλητικό event, ίδιο μουσικό event/άλμπουμ, ίδια δικαστική υπόθεση.
- ΟΧΙ σε γενικές κατηγορίες: μη βάζεις μαζί άρθρα που απλά έχουν ίδια λέξη-κλειδί αλλά διαφορετικό γεγονός.
- Ο τίτλος topic να είναι στα ΕΛΛΗΝΙΚΑ. Απαγορεύονται αγγλικοί τίτλοι.
- Το category πρέπει να είναι είτε "ειδήσεις Κόσμος" είτε "ειδήσεις Ελλάδα".
- Τα article_indices ξεκινάνε από 0 για το πρώτο άρθρο.

Επιστρέψε ΜΟΝΟ JSON:
{"groups": [{"topic": "Ελληνικός τίτλος θέματος", "category": "ειδήσεις Κόσμος", "article_indices": [0, 3, 7], "sentence": "..."}]}

Άρθρα:
`)
	sb.WriteString(fmt.Sprintf("(Σύνολο %d άρθρα):\n", len(arts)))
	for i, a := range arts {
		sb.WriteString(fmt.Sprintf("[%d] %s — %s (πηγή: %s)\n", i, a.Title, a.Feed, a.Category))
	}

	prompt := sb.String()
	jsonStr, err := s.Gemini.CallGrouping(prompt)
	if err != nil {
		return nil, err
	}

	var batchResult struct {
		Groups []struct {
			Topic          string `json:"topic"`
			Category       string `json:"category"`
			ArticleIndices []int  `json:"article_indices"`
			Sentence       string `json:"sentence"`
		} `json:"groups"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &batchResult); err != nil {
		return nil, fmt.Errorf("parse merged groups: %w", err)
	}

	// Separate groups by assigned category
	type topicGroup struct {
		models.AIGroup
		indices map[int]bool
	}
	var worldGroups, greekGroups []topicGroup
	worldUsed := make(map[int]bool)
	greekUsed := make(map[int]bool)

	for _, g := range batchResult.Groups {
		var grouped []models.Article
		idxSet := make(map[int]bool)
		for _, idx := range g.ArticleIndices {
			if idx >= 0 && idx < len(arts) {
				grouped = append(grouped, arts[idx])
				idxSet[idx] = true
			}
		}
		if len(grouped) == 0 {
			continue
		}
		tg := topicGroup{
			AIGroup: models.AIGroup{Topic: g.Topic, Articles: grouped, Count: len(grouped)},
			indices: idxSet,
		}
		if g.Category == "ειδήσεις Κόσμος" {
			for idx := range idxSet {
				worldUsed[idx] = true
			}
			worldGroups = append(worldGroups, tg)
		} else {
			for idx := range idxSet {
				greekUsed[idx] = true
			}
			greekGroups = append(greekGroups, tg)
		}
	}

	// Remaining (ungrouped) articles: place based on original category
	var worldSingles, greekSingles []models.AIGroup
	for i, a := range arts {
		if worldUsed[i] || greekUsed[i] {
			continue
		}
		if a.Category == "international" {
			worldSingles = append(worldSingles, models.AIGroup{Topic: a.Title, Articles: []models.Article{a}, Count: 1})
		} else {
			// news or politics → "ειδήσεις Ελλάδα"
			greekSingles = append(greekSingles, models.AIGroup{Topic: a.Title, Articles: []models.Article{a}, Count: 1})
		}
	}

	result := make(map[string]models.AICategoryGroup)
	if len(worldGroups) > 0 || len(worldSingles) > 0 {
		var allGroups []models.AIGroup
		for _, tg := range worldGroups {
			allGroups = append(allGroups, tg.AIGroup)
		}
		allGroups = append(allGroups, worldSingles...)
		worldCat := models.AICategoryGroup{
			Category: "ειδήσεις Κόσμος",
			Groups:   allGroups,
			Count:    CountArticles(allGroups),
		}
		result["ειδήσεις Κόσμος"] = worldCat
	}
	if len(greekGroups) > 0 || len(greekSingles) > 0 {
		var allGroups []models.AIGroup
		for _, tg := range greekGroups {
			allGroups = append(allGroups, tg.AIGroup)
		}
		allGroups = append(allGroups, greekSingles...)
		greekCat := models.AICategoryGroup{
			Category: "ειδήσεις Ελλάδα",
			Groups:   allGroups,
			Count:    CountArticles(allGroups),
		}
		result["ειδήσεις Ελλάδα"] = greekCat
	}

	return result, nil
}
