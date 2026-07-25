package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

// ---------- config ----------

// catDisplayName maps internal Miniflux category names to Greek display names
var catDisplayName = map[string]string{
	"international": "ειδήσεις Κόσμος",
	"news":          "ειδήσεις Ελλάδα",
	"sports":        "αθλητικές ειδήσεις",
	"music":         "μουσικά νέα",
	"politics":      "ειδήσεις Ελλάδα",
	"random":        "διάφορες ειδήσεις",
	"panionara":     "αθλητικές ειδήσεις",
}

type config struct {
	dbHost       string
	dbPort       int
	dbName       string
	dbUser       string
	dbPass       string
	sshHost      string
	sshRemote    int
	sshTunnel    int
	httpPort     int
	geminiAPIKey string
}

func loadConfig() config {
	return config{
		dbHost:       envStr("MF_DB_HOST", "127.0.0.1"),
		dbPort:       envInt("MF_DB_PORT", 5433),
		dbName:       envStr("MF_DB_NAME", "miniflux"),
		dbUser:       envStr("MF_DB_USER", "external_reader"),
		dbPass:       envStr("MF_DB_PASSWORD", ""),
		sshHost:      envStr("MF_SSH_HOST", ""),
		sshRemote:    envInt("MF_SSH_REMOTE_PORT", 5435),
		sshTunnel:    envInt("MF_SSH_TUNNEL_PORT", 5433),
		httpPort:     envInt("PORT", 8080),
		geminiAPIKey: envStr("GEMINI_API_KEY", ""),
	}
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

// ---------- SSH tunnel ----------

type tunnel struct {
	cmd *exec.Cmd
	mu  sync.Mutex
}

func (t *tunnel) open(cfg config) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if cfg.sshHost == "" {
		log.Println("No SSH host configured — connecting directly to DB")
		return nil
	}

	_ = exec.Command("pkill", "-f", fmt.Sprintf("ssh.*-L %d:", cfg.sshTunnel)).Run()
	time.Sleep(500 * time.Millisecond)

	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentityFile=/root/.ssh/id_rsa",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-F", "/dev/null",
		"-L", fmt.Sprintf("%d:127.0.0.1:%d", cfg.sshTunnel, cfg.sshRemote),
		"-N",
		cfg.sshHost,
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("SSH tunnel start failed: %w", err)
	}
	t.cmd = cmd
	// Wait for tunnel to be ready
	time.Sleep(3 * time.Second)
	// Verify tunnel is actually listening
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.sshTunnel), 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("SSH tunnel not listening after start: %w", err)
	}
	log.Printf("SSH tunnel open: localhost:%d → %s:%d", cfg.sshTunnel, cfg.sshHost, cfg.sshRemote)
	return nil
}

func (t *tunnel) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = exec.Command("pkill", "-f", fmt.Sprintf("ssh.*-L %d:", 5433)).Run()
}

// ---------- database types ----------

type article struct {
	ID        int64
	Title     string
	URL       string
	Published time.Time
	Feed      string
	Category  string
	TimeAgo   string
}

type categoryGroup struct {
	Category string
	Articles []article
	Count    int
}

type dashboardData struct {
	Categories []categoryGroup
	Total      int
	Generated  string
}

// ---------- AI types ----------

type aiGroup struct {
	Topic    string    `json:"topic"`
	Articles []article `json:"articles"`
	Count    int       `json:"count"`
	GroupID  string    `json:"group_id,omitempty"`
}

type aiCategoryGroup struct {
	Category string    `json:"category"`
	Groups   []aiGroup `json:"groups"`
	Count    int       `json:"count"`
}

type aiDashboardData struct {
	Categories []aiCategoryGroup `json:"categories"`
	Total      int               `json:"total"`
	Generated  string            `json:"generated"`
}

// ---------- Gemini API ----------

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
	Config   geminiConfig    `json:"generationConfig"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiConfig struct {
	Temperature     float64 `json:"temperature"`
	MaxOutputTokens int     `json:"maxOutputTokens"`
	ResponseMIME    string  `json:"responseMimeType"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
}

func (s *server) callGemini(prompt string) (string, error) {
	return s.callGeminiWithModel(prompt, "gemini-2.5-flash-lite", 8192)
}

func (s *server) callGeminiGrouping(prompt string) (string, error) {
	return s.callGeminiWithModel(prompt, "gemini-2.5-flash", 65536)
}

func (s *server) callGeminiWithModel(prompt, model string, maxTokens int) (string, error) {
	if s.cfg.geminiAPIKey == "" {
		return "", fmt.Errorf("GEMINI_API_KEY not configured")
	}

	req := geminiRequest{
		Contents: []geminiContent{
			{Parts: []geminiPart{{Text: prompt}}},
		},
		Config: geminiConfig{
			Temperature:     0.2,
			MaxOutputTokens: maxTokens,
			ResponseMIME:    "application/json",
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, s.cfg.geminiAPIKey)

	resp, err := s.httpClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("gemini API call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("gemini API error %d: %s", resp.StatusCode, string(respBody))
	}

	var gr geminiResponse
	if err := json.Unmarshal(respBody, &gr); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("no candidates in Gemini response")
	}

	return strings.TrimSpace(gr.Candidates[0].Content.Parts[0].Text), nil
}

// ---------- AI grouping ----------

func (s *server) aiGroupArticles(allArticles []article) ([]aiCategoryGroup, error) {
	tTotal := time.Now()
	
	// Create command to run Python clustering script inside standard path
	cmd := exec.Command("/opt/venv/bin/python3", "cluster_articles.py", "6")
	
	// Set environment variables for DB access
	cmd.Env = os.Environ()
	
	// Execute and collect stdout
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = os.Stderr // Let warnings print to container stderr, don't mix into json!
	err := cmd.Run()
	out := outBuf.Bytes()
	if err != nil {
		log.Printf("[clustering] python subprocess failed: %v, output: %s", err, string(out))
		// Fallback to flat view
		catMap := make(map[string][]article)
		for _, a := range allArticles {
			catMap[a.Category] = append(catMap[a.Category], a)
		}
		preferredOrder := []string{"international", "news", "sports", "music", "politics", "random", "panionara"}
		return flatGroupResult(catMap, preferredOrder, catDisplayName), nil
	}

	var data aiDashboardData
	if err := json.Unmarshal(out, &data); err != nil {
		log.Printf("[clustering] failed to unmarshal python output: %v", err)
		// Fallback to flat view
		catMap := make(map[string][]article)
		for _, a := range allArticles {
			catMap[a.Category] = append(catMap[a.Category], a)
		}
		preferredOrder := []string{"international", "news", "sports", "music", "politics", "random", "panionara"}
		return flatGroupResult(catMap, preferredOrder, catDisplayName), nil
	}

	log.Printf("[clustering] Local Embeddings clustering completed in %v", time.Since(tTotal))
	return data.Categories, nil
}

func (s *server) aiGroupCategory(cat string, arts []article, displayName string) (aiCategoryGroup, error) {
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
	jsonStr, err := s.callGeminiGrouping(prompt)
	if err != nil {
		return aiCategoryGroup{}, err
	}

	var batchResult struct {
		Groups []struct {
			Topic          string `json:"topic"`
			ArticleIndices []int  `json:"article_indices"`
			Sentence       string `json:"sentence"`
		} `json:"groups"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &batchResult); err != nil {
		return aiCategoryGroup{}, fmt.Errorf("parse groups: %w", err)
	}

	return buildAICatGroupFromParsed(cat, arts, displayName, batchResult.Groups), nil
}

// aiGroupWithCategory processes articles from "news", "politics", "music" categories.
// It asks Gemini to group by topic AND assign each group to the correct
// display category (e.g. "ειδήσεις Κόσμος", "ειδήσεις Ελλάδα", "μουσικά νέα", "αθλητικές ειδήσεις").
// Returns a map[displayCategory][]aiGroup.
func (s *server) aiGroupWithCategory(cat string, arts []article) (map[string][]aiGroup, error) {
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
`, catDisplayName[cat], len(arts)))
	for i, a := range arts {
		sb.WriteString(fmt.Sprintf("[%d] %s — %s\n", i, a.Title, a.Feed))
	}

	prompt := sb.String()
	jsonStr, err := s.callGeminiGrouping(prompt)
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
	result := map[string][]aiGroup{
		"ειδήσεις Κόσμος":     {},
		"ειδήσεις Ελλάδα":    {},
		"μουσικά νέα":         {},
		"αθλητικές ειδήσεις":   {},
		"διάφορες ειδήσεις":   {},
	}
	used := make(map[int]bool)

	for _, g := range batchResult.Groups {
		var grouped []article
		for _, idx := range g.ArticleIndices {
			if idx >= 0 && idx < len(arts) && !used[idx] {
				grouped = append(grouped, arts[idx])
				used[idx] = true
			}
		}
		if len(grouped) == 0 {
			continue
		}
		ag := aiGroup{Topic: g.Topic, Articles: grouped, Count: len(grouped)}
		target := catDisplayName[cat] // default to original display name
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
			dName := catDisplayName[cat]
			if dName == "" {
				dName = "διάφορες ειδήσεις"
			}
			result[dName] = append(result[dName], aiGroup{
				Topic: a.Title, Articles: []article{a}, Count: 1,
			})
		}
	}

	return result, nil
}

// aiGroupMergedCategory sends a combined batch of articles from news+politics+international
// to Gemini, which returns topic groups EACH with an assigned display category
// ("ειδήσεις Κόσμος" or "ειδήσεις Ελλάδα"). Groups are separated into the correct
// aiCategoryGroup entries, and ungrouped articles are placed based on their original
// Miniflux category.
func (s *server) aiGroupMergedCategory(arts []article) (map[string]aiCategoryGroup, error) {
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
	jsonStr, err := s.callGeminiGrouping(prompt)
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
		aiGroup
		indices map[int]bool
	}
	var worldGroups, greekGroups []topicGroup
	worldUsed := make(map[int]bool)
	greekUsed := make(map[int]bool)

	for _, g := range batchResult.Groups {
		var grouped []article
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
			aiGroup: aiGroup{Topic: g.Topic, Articles: grouped, Count: len(grouped)},
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
	var worldSingles, greekSingles []aiGroup
	for i, a := range arts {
		if worldUsed[i] || greekUsed[i] {
			continue
		}
		if a.Category == "international" {
			worldSingles = append(worldSingles, aiGroup{Topic: a.Title, Articles: []article{a}, Count: 1})
		} else {
			// news or politics → "ειδήσεις Ελλάδα"
			greekSingles = append(greekSingles, aiGroup{Topic: a.Title, Articles: []article{a}, Count: 1})
		}
	}

	result := make(map[string]aiCategoryGroup)
	if len(worldGroups) > 0 || len(worldSingles) > 0 {
		var allGroups []aiGroup
		for _, tg := range worldGroups {
			allGroups = append(allGroups, tg.aiGroup)
		}
		allGroups = append(allGroups, worldSingles...)
		worldCat := aiCategoryGroup{
			Category: "ειδήσεις Κόσμος",
			Groups:   allGroups,
			Count:    countArticles(allGroups),
		}
		result["ειδήσεις Κόσμος"] = worldCat
	}
	if len(greekGroups) > 0 || len(greekSingles) > 0 {
		var allGroups []aiGroup
		for _, tg := range greekGroups {
			allGroups = append(allGroups, tg.aiGroup)
		}
		allGroups = append(allGroups, greekSingles...)
		greekCat := aiCategoryGroup{
			Category: "ειδήσεις Ελλάδα",
			Groups:   allGroups,
			Count:    countArticles(allGroups),
		}
		result["ειδήσεις Ελλάδα"] = greekCat
	}

	return result, nil
}

// groupBy partitions a slice by a key function (generic-style helper).
func groupBy[T any](items []T, keyFn func(T) string) map[string][]T {
	m := make(map[string][]T)
	for _, item := range items {
		k := keyFn(item)
		m[k] = append(m[k], item)
	}
	return m
}

// countArticles sums the Count field across a slice of aiGroup.
func countArticles(groups []aiGroup) int {
	n := 0
	for _, g := range groups {
		n += g.Count
	}
	return n
}

// countCatGroups counts total groups across all categories.
func countCatGroups(cats []aiCategoryGroup) int {
	n := 0
	for _, c := range cats {
		n += len(c.Groups)
	}
	return n
}

func buildAICatGroupFlat(cat string, arts []article, displayName string) aiCategoryGroup {
	acg := aiCategoryGroup{Category: displayName, Count: len(arts)}
	for _, a := range arts {
		acg.Groups = append(acg.Groups, aiGroup{Topic: a.Title, Articles: []article{a}, Count: 1})
	}
	return acg
}

func buildAICatGroupFromParsed(cat string, arts []article, displayName string, groups []struct {
	Topic          string `json:"topic"`
	ArticleIndices []int  `json:"article_indices"`
	Sentence       string `json:"sentence"`
}) aiCategoryGroup {
	acg := aiCategoryGroup{Category: displayName, Count: len(arts)}
	used := make(map[int]bool)
	if len(groups) > 0 {
		for _, g := range groups {
			var grouped []article
			for _, idx := range g.ArticleIndices {
				if idx >= 0 && idx < len(arts) && !used[idx] {
					grouped = append(grouped, arts[idx])
					used[idx] = true
				}
			}
			if len(grouped) > 0 {
				acg.Groups = append(acg.Groups, aiGroup{Topic: g.Topic, Articles: grouped, Count: len(grouped)})
			}
		}
	}
	// Add remaining (ungrouped) articles as singletons
	for i, a := range arts {
		if !used[i] {
			acg.Groups = append(acg.Groups, aiGroup{Topic: a.Title, Articles: []article{a}, Count: 1})
		}
	}
	return acg
}

func flatGroupResult(catMap map[string][]article, preferredOrder []string, catDisplayName map[string]string) []aiCategoryGroup {
	var result []aiCategoryGroup
	for _, cat := range preferredOrder {
		if arts, ok := catMap[cat]; ok {
			displayName := catDisplayName[cat]
			if displayName == "" {
				displayName = cat
			}
			acg := aiCategoryGroup{Category: displayName, Count: len(arts)}
			for _, a := range arts {
				acg.Groups = append(acg.Groups, aiGroup{Topic: a.Title, Articles: []article{a}, Count: 1})
			}
			result = append(result, acg)
			delete(catMap, cat)
		}
	}
	for cat, arts := range catMap {
		displayName := catDisplayName[cat]
		if displayName == "" {
			displayName = cat
		}
		acg := aiCategoryGroup{Category: displayName, Count: len(arts)}
		for _, a := range arts {
			acg.Groups = append(acg.Groups, aiGroup{Topic: a.Title, Articles: []article{a}, Count: 1})
		}
		result = append(result, acg)
	}
	return result
}

// ---------- AI summarization ----------

type aiSummaryResult struct {
	Summary string   `json:"summary"`
	Sources []string `json:"sources"`
}

func (s *server) aiSummarize(topic string, articles []article) (aiSummaryResult, error) {
	var sb strings.Builder

	// 50 λέξεις ανά πηγή, max 500
	wordLimit := len(articles) * 50
	if wordLimit > 500 {
		wordLimit = 500
	}
	if wordLimit < 50 {
		wordLimit = 50
	}

	template := fmt.Sprintf(`Είσαι βοηθός σύνοψης ειδήσεων. Θέμα: "%s"

Παρακάτω είναι άρθρα που καλύπτουν αυτό το θέμα. 
1. Γράψε μια σύντομη περίληψη (έως %d λέξεις, στα ΕΛΛΗΝΙΚΑ) που να εξηγεί τι συνέβη.
2. Επίσης, γράψε έναν κατάλληλο, ελκυστικό και σύντομο τίτλο θεματικής (Topic) στα ΕΛΛΗΝΙΚΑ για αυτό το γκρουπ ειδήσεων, ο οποίος να αντικατοπτρίζει σωστά το περιεχόμενο.

Return JSON: {"summary": "Περίληψη...", "topic": "Ελληνικός Τίτλος Θέματος"}

Άρθρα:
`, topic, wordLimit)
	sb.WriteString(template)

	for i, a := range articles {
		sb.WriteString(fmt.Sprintf("[%d] Title: %s\n   Source: %s (%s)\n   URL: %s\n\n", i+1, a.Title, a.Feed, a.Category, a.URL))
	}

	jsonStr, err := s.callGemini(sb.String())
	if err != nil {
		return aiSummaryResult{}, fmt.Errorf("summarize call failed: %w", err)
	}

	type geminiSummaryResponse struct {
		Summary string `json:"summary"`
		Topic   string `json:"topic"`
	}

	var parsed geminiSummaryResponse
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		// Fallback parse if Gemini failed to return expected keys
		var backup aiSummaryResult
		if backupErr := json.Unmarshal([]byte(jsonStr), &backup); backupErr == nil && backup.Summary != "" {
			return backup, nil
		}
		return aiSummaryResult{}, fmt.Errorf("parse summary: %w", err)
	}

	return aiSummaryResult{
		Summary: parsed.Summary,
		Sources: []string{parsed.Topic}, // We'll put the topic name here to pass it up, or let the handler handle it
	}, nil
}

// ---------- AI cache ----------

type aiCache struct {
	mu       sync.RWMutex
	data     *aiDashboardData
	ts       time.Time
	interval time.Duration
}

func newAICache(interval time.Duration) *aiCache {
	return &aiCache{interval: interval}
}

func (c *aiCache) get(db *sql.DB, s *server) (*aiDashboardData, error) {
	c.mu.RLock()
	if c.data != nil && time.Since(c.ts) < c.interval {
		d := c.data
		c.mu.RUnlock()
		return d, nil
	}
	// Cache expired or empty. If we have stale data, return it immediately
	// and start a background refresh so the next request gets fresh data.
	if c.data != nil {
		stale := c.data
		c.mu.RUnlock()
		// Background refresh (non-blocking)
		go c.refresh(db, s)
		return stale, nil
	}
	c.mu.RUnlock()

	// No stale data available — do a blocking fetch (cold start)
	c.mu.Lock()
	// Double-check
	if c.data != nil {
		c.mu.Unlock()
		return c.data, nil
	}
	c.mu.Unlock()

	return c.refreshBlocking(db, s)
}

// refresh fetches fresh AI data in the background and updates the cache.
func (c *aiCache) refresh(db *sql.DB, s *server) {
	c.mu.Lock()
	// Another goroutine already refreshed it
	if c.data != nil && time.Since(c.ts) < c.interval {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	log.Println("AI cache stale — refreshing in background")
	newData, err := c.fetchFresh(db, s)
	if err != nil {
		log.Printf("AI background refresh failed, keeping stale: %v", err)
		return
	}

	c.mu.Lock()
	c.data = newData
	c.ts = time.Now()
	c.mu.Unlock()
	log.Printf("AI cache refreshed — %d articles in %d categories", newData.Total, len(newData.Categories))
}

// refreshBlocking does a cold-start blocking fetch (no stale data to serve).
func (c *aiCache) refreshBlocking(db *sql.DB, s *server) (*aiDashboardData, error) {
	newData, err := c.fetchFresh(db, s)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.data = newData
	c.ts = time.Now()
	c.mu.Unlock()
	return newData, nil
}

// fetchFresh queries the DB and runs AI grouping. No locking — caller handles it.
func (c *aiCache) fetchFresh(db *sql.DB, s *server) (*aiDashboardData, error) {
	log.Printf("[timing] fetchFresh: starting DB query")
	dd, err := s.dbQueryWithRetry(12)
	if err != nil {
		return nil, err
	}
	log.Printf("[timing] fetchFresh: DB query done — %d categories", len(dd.Categories))

	var allArticles []article
	for _, cat := range dd.Categories {
		allArticles = append(allArticles, cat.Articles...)
	}
	log.Printf("[timing] fetchFresh: %d total articles", len(allArticles))

	if len(allArticles) == 0 {
		return &aiDashboardData{Categories: []aiCategoryGroup{}, Total: 0, Generated: time.Now().Format("15:04:05")}, nil
	}

	t0 := time.Now()
	groups, err := s.aiGroupArticles(allArticles)
	if err != nil {
		return nil, err
	}
	log.Printf("[timing] fetchFresh: AI grouping done in %v — %d categories with %d groups total", time.Since(t0).Round(time.Second), len(groups), countCatGroups(groups))

	// Assign stable group IDs (hash of sorted article IDs) so the frontend
	// can reference groups by ID instead of index. This prevents the
	// "click one article, get another" bug when the cache refreshes.
	assignGroupIDs(groups)

	total := 0
	for _, g := range groups {
		total += g.Count
	}

	return &aiDashboardData{Categories: groups, Total: total, Generated: time.Now().Format("15:04:05")}, nil
}

func (c *aiCache) invalidate() {
	c.mu.Lock()
	c.ts = time.Time{}
	c.mu.Unlock()
}

// assignGroupIDs computes stable, content-based IDs for each group.
// Uses SHA-256 of sorted article IDs so the same group of articles
// always gets the same ID regardless of clustering order.
func assignGroupIDs(categories []aiCategoryGroup) {
	totalGroups := 0
	for ci := range categories {
		for gi := range categories[ci].Groups {
			g := &categories[ci].Groups[gi]
			if len(g.Articles) == 0 {
				continue
			}
			ids := make([]int64, len(g.Articles))
			for i, a := range g.Articles {
				ids[i] = a.ID
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			h := sha256.New()
			for _, id := range ids {
				h.Write([]byte(fmt.Sprintf("%d,", id)))
			}
			g.GroupID = hex.EncodeToString(h.Sum(nil))[:12]
			totalGroups++
		}
	}
	log.Printf("[groupID] Assigned IDs to %d groups", totalGroups)
}

// ---------- database queries ----------

func openDB(cfg config) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable connect_timeout=5",
		cfg.dbHost, cfg.dbPort, cfg.dbUser, cfg.dbPass, cfg.dbName,
	)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}

func (s *server) dbQueryWithRetry(hours int) (*dashboardData, error) {
	data, err := s.queryArticles(hours)
	if err != nil && (strings.Contains(err.Error(), "i/o timeout") || strings.Contains(err.Error(), "connection refused")) {
		log.Printf("DB connection lost, reconnecting tunnel: %v", err)
		s.tunnel.close()
		time.Sleep(1 * time.Second)
		if tunErr := s.tunnel.open(s.cfg); tunErr != nil {
			log.Printf("Tunnel reopen failed: %v", tunErr)
		} else if pingErr := s.db.Ping(); pingErr == nil {
			log.Println("Tunnel reconnected, retrying query")
			time.Sleep(2 * time.Second) // wait for SSH tunnel to be ready
			data, err = s.queryArticles(hours)
		}
		// One more retry if still failing
		if err != nil {
			time.Sleep(3 * time.Second)
			s.tunnel.close()
			if tunErr := s.tunnel.open(s.cfg); tunErr == nil {
				time.Sleep(2 * time.Second)
				data, err = s.queryArticles(hours)
			}
		}
	}
	return data, err
}

func (s *server) queryArticles(hours int) (*dashboardData, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour)

	rows, err := s.db.Query(`
		SELECT e.id, e.title, e.url, e.published_at, f.title AS feed, c.title AS category
		FROM entries e
		JOIN feeds f ON f.id = e.feed_id
		JOIN categories c ON c.id = f.category_id
		WHERE e.published_at >= $1
		  AND (f.disabled = false OR f.disabled IS NULL)
		ORDER BY e.published_at DESC
	`, since)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type entry struct {
		id        int64
		title     string
		url       string
		published time.Time
		feed      string
		category  string
	}
	seen := make(map[string]*entry)
	var ordered []string

	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.title, &e.url, &e.published, &e.feed, &e.category); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		e.title = strings.TrimSpace(e.title)
		if _, exists := seen[e.title]; !exists {
			seen[e.title] = &e
			ordered = append(ordered, e.title)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iter: %w", err)
	}

	catMap := make(map[string][]article)
	for _, title := range ordered {
		e := seen[title]
		art := article{
			ID:        e.id,
			Title:     e.title,
			URL:       e.url,
			Published: e.published,
			Feed:      e.feed,
			Category:  e.category,
			TimeAgo:   timeAgo(e.published),
		}
		catMap[e.category] = append(catMap[e.category], art)
	}

	preferredOrder := []string{"international", "news", "sports", "music", "politics", "random", "panionara"}
	var groups []categoryGroup
	for _, cat := range preferredOrder {
		if arts, ok := catMap[cat]; ok {
			groups = append(groups, categoryGroup{Category: cat, Articles: arts, Count: len(arts)})
			delete(catMap, cat)
		}
	}
	for cat, arts := range catMap {
		groups = append(groups, categoryGroup{Category: cat, Articles: arts, Count: len(arts)})
	}

	return &dashboardData{Categories: groups, Total: len(ordered), Generated: time.Now().Format("15:04:05")}, nil
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		return t.Format("Jan 2 15:04")
	}
}

// ---------- article cache ----------

type cache struct {
	mu       sync.RWMutex
	data     *dashboardData
	ts       time.Time
	interval time.Duration
}

func newCache(interval time.Duration) *cache {
	return &cache{interval: interval}
}

func (c *cache) get(s *server) (*dashboardData, error) {
	c.mu.RLock()
	if c.data != nil && time.Since(c.ts) < c.interval {
		d := c.data
		c.mu.RUnlock()
		return d, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.data != nil && time.Since(c.ts) < c.interval {
		return c.data, nil
	}

	data, err := s.dbQueryWithRetry(6)
	if err != nil {
		if c.data != nil {
			log.Printf("DB query failed, serving stale data: %v", err)
			return c.data, nil
		}
		return nil, err
	}
	c.data = data
	c.ts = time.Now()
	return data, nil
}

func (c *cache) invalidate() {
	c.mu.Lock()
	c.ts = time.Time{}
	c.mu.Unlock()
}

// ---------- templates ----------

//go:embed templates static
var embeddedFS embed.FS

type templateSet struct {
	base    *template.Template
	partial *template.Template
}

func loadTemplates() (*templateSet, error) {
	funcMap := template.FuncMap{
		"upper": strings.ToUpper,
		"catIcon": func(cat string) string {
			switch {
			case strings.Contains(cat, "μουσικ"):
				return "🎵"
			case strings.Contains(cat, "αθλητ"):
				return "⚽"
			case strings.Contains(cat, "Κόσμος"):
				return "🌍"
			case strings.Contains(cat, "Ελλάδα"):
				return "🇬🇷"
			case strings.Contains(cat, "διάφορ"):
				return "🌀"
			default:
				return "📰"
			}
		},
		"uniqueFeeds": func(articles []article) []string {
			seen := make(map[string]bool)
			var unique []string
			for _, a := range articles {
				if !seen[a.Feed] {
					seen[a.Feed] = true
					unique = append(unique, a.Feed)
				}
			}
			return unique
		},
	}

	tpl := template.New("").Funcs(funcMap)
	err := fs.WalkDir(embeddedFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".tmpl") {
			content, err := embeddedFS.ReadFile(path)
			if err != nil {
				return err
			}
			_, err = tpl.Parse(string(content))
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &templateSet{base: tpl, partial: tpl}, nil
}

// ---------- flat-to-AI conversion ----------

func flatToAI(dd *dashboardData) *aiDashboardData {
	var cats []aiCategoryGroup
	for _, cg := range dd.Categories {
		displayName := catDisplayName[cg.Category]
		if displayName == "" {
			displayName = cg.Category
		}
		acg := aiCategoryGroup{Category: displayName, Count: cg.Count}
		for _, a := range cg.Articles {
			acg.Groups = append(acg.Groups, aiGroup{Topic: a.Title, Articles: []article{a}, Count: 1})
		}
		cats = append(cats, acg)
	}
	return &aiDashboardData{Categories: cats, Total: dd.Total, Generated: dd.Generated}
}

// ---------- HTTP handlers ----------

type server struct {
	cfg        config
	db         *sql.DB
	tpl        *templateSet
	cache      *cache
	aiCache    *aiCache
	tunnel     *tunnel
	httpClient *http.Client
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "HX-Request")
	w.Header().Set("Cache-Control", "no-cache")

	aiData, err := s.aiCache.get(s.db, s)
	if err != nil {
		log.Printf("AI data error (falling back to flat): %v", err)
		flat, flatErr := s.cache.get(s)
		if flatErr != nil {
			http.Error(w, "Data error", http.StatusInternalServerError)
			return
		}
		aiData = flatToAI(flat)
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Push-Url", "/")
		_ = s.tpl.partial.ExecuteTemplate(w, "articles", aiData)
		return
	}

	_ = s.tpl.base.ExecuteTemplate(w, "base", aiData)
}

func (s *server) categoryFilter(w http.ResponseWriter, r *http.Request) {
	cat := r.URL.Query().Get("cat")
	data, err := s.aiCache.get(s.db, s)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Vary", "HX-Request")
	w.Header().Set("Cache-Control", "no-cache")

	var filtered aiDashboardData
	if cat == "" || cat == "all" {
		filtered = *data
	} else {
		for _, g := range data.Categories {
			if strings.EqualFold(g.Category, cat) {
				filtered.Categories = append(filtered.Categories, g)
				filtered.Total = g.Count
			}
		}
	}
	filtered.Generated = data.Generated

	// HTMX request: return only the articles partial
	if r.Header.Get("HX-Request") == "true" {
		_ = s.tpl.partial.ExecuteTemplate(w, "articles", filtered)
		return
	}
	// Full page load: render base template with filtered data
	_ = s.tpl.base.ExecuteTemplate(w, "base", filtered)
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	data, err := s.aiCache.get(s.db, s)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")

	if q == "" {
		if r.Header.Get("HX-Request") == "true" {
			_ = s.tpl.partial.ExecuteTemplate(w, "articles", data)
			return
		}
		_ = s.tpl.base.ExecuteTemplate(w, "base", data)
		return
	}

	var filtered aiDashboardData
	filtered.Generated = data.Generated
	for _, g := range data.Categories {
		var matchedGroups []aiGroup
		for _, grp := range g.Groups {
			var matched []article
			for _, a := range grp.Articles {
				if strings.Contains(strings.ToLower(a.Title), q) ||
					strings.Contains(strings.ToLower(a.Feed), q) {
					matched = append(matched, a)
				}
			}
			if len(matched) > 0 {
				matchedGroups = append(matchedGroups, aiGroup{Topic: grp.Topic, Articles: matched, Count: len(matched)})
				filtered.Total += len(matched)
			}
		}
		if len(matchedGroups) > 0 {
			filtered.Categories = append(filtered.Categories, aiCategoryGroup{Category: g.Category, Groups: matchedGroups, Count: len(matchedGroups)})
		} else {
			filtered.Categories = append(filtered.Categories, aiCategoryGroup{Category: g.Category, Count: 0})
		}
	}

	// HTMX request: return only matched articles partial
	if r.Header.Get("HX-Request") == "true" {
		_ = s.tpl.partial.ExecuteTemplate(w, "articles", filtered)
		return
	}
	// Full page load: render base template with matched data
	_ = s.tpl.base.ExecuteTemplate(w, "base", filtered)
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	data, err := s.aiCache.get(s.db, s)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	_ = s.tpl.partial.ExecuteTemplate(w, "stats-old", dashboardData{Total: data.Total, Generated: data.Generated})
}

func (s *server) handleSummarize(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cat := r.FormValue("category")
	groupID := r.FormValue("group_id")
	groupIdxStr := r.FormValue("group")

	groupIdx, err := strconv.Atoi(groupIdxStr)
	if err != nil {
		groupIdx = -1
	}

	data, err := s.aiCache.get(s.db, s)
	if err != nil {
		http.Error(w, "Data error", http.StatusInternalServerError)
		return
	}

	var targetGroup aiGroup
	var targetTopic string
	found := false

	// Prefer lookup by stable group_id (prevents index-mismatch on cache refresh)
	for _, cg := range data.Categories {
		if !strings.EqualFold(cg.Category, cat) {
			continue
		}
		if groupID != "" {
			for _, g := range cg.Groups {
				if g.GroupID == groupID {
					targetGroup = g
					targetTopic = g.Topic
					found = true
					break
				}
			}
		}
		// Fallback to index lookup ONLY if no group_id was provided (legacy clients)
		if !found && groupID == "" && groupIdx >= 0 && groupIdx < len(cg.Groups) {
			targetGroup = cg.Groups[groupIdx]
			targetTopic = cg.Groups[groupIdx].Topic
			found = true
		}
		if found {
			break
		}
	}

	if !found {
		// group_id provided but not found = page data is stale (cache refresh changed groups)
		// Return a friendly HTMX fragment so the user knows to refresh, instead of serving wrong data
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<div class="summary-refresh">⚠️ Τα δεδομένα άλλαξαν. Κάνε <a href="/" hx-get="/" hx-target="body">ανανέωση</a> και δοκίμασε ξανά.</div>`))
		return
	}

	// No AI key — show fallback directly
	if s.cfg.geminiAPIKey == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.tpl.partial.ExecuteTemplate(w, "summary-fallback", targetGroup)
		return
	}

	summary, err := s.aiSummarize(targetTopic, targetGroup.Articles)
	if err != nil {
		log.Printf("Summarize error: %v", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.tpl.partial.ExecuteTemplate(w, "summary-fallback", targetGroup)
		return
	}

	// Update cache data so the newly generated fancy topic persists in the cluster object
	if len(summary.Sources) > 0 && summary.Sources[0] != "" {
		fancyTopic := summary.Sources[0]
		targetTopic = fancyTopic
		
		// Write-update the cached topic name, so if user closes/re-opens it retains the AI name
		s.aiCache.mu.Lock()
		for catIdx, cg := range data.Categories {
			if strings.EqualFold(cg.Category, cat) {
				if groupIdx >= 0 && groupIdx < len(cg.Groups) {
					data.Categories[catIdx].Groups[groupIdx].Topic = fancyTopic
				}
			}
		}
		s.aiCache.mu.Unlock()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tpl.partial.ExecuteTemplate(w, "summary", map[string]interface{}{
		"Topic":    targetTopic,
		"Summary":  summary.Summary,
		"Articles": targetGroup.Articles,
		"Category": cat,
	})
}

// ---------- static files ----------

func staticHandler() http.Handler {
	staticFS, err := fs.Sub(embeddedFS, "static")
	if err != nil {
		log.Fatalf("static sub fs: %v", err)
	}
	return http.FileServer(http.FS(staticFS))
}

// ---------- main ----------

func main() {
	cfg := loadConfig()

	t := &tunnel{}
	if err := t.open(cfg); err != nil {
		log.Fatalf("Failed to open SSH tunnel: %v", err)
	}
	defer t.close()

	db, err := openDB(cfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("Database ping failed: %v", err)
	}
	log.Println("Database connected successfully")

	tpl, err := loadTemplates()
	if err != nil {
		log.Fatalf("Failed to load templates: %v", err)
	}

	if cfg.geminiAPIKey == "" {
		log.Println("WARNING: GEMINI_API_KEY not set — AI features disabled")
	}

	// HTTP client with higher concurrent connection limits for parallel Gemini calls
	httpClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        50,
			MaxIdleConnsPerHost: 20,
			MaxConnsPerHost:     20,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 300 * time.Second,
	}

	srv := &server{
		cfg:        cfg,
		db:         db,
		tpl:        tpl,
		cache:      newCache(2 * time.Minute),
		aiCache:    newAICache(60 * time.Minute),
		tunnel:     t,
		httpClient: httpClient,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", srv.home)
	mux.HandleFunc("GET /filter", srv.categoryFilter)
	mux.HandleFunc("GET /search", srv.search)
	mux.HandleFunc("GET /stats", srv.stats)
	mux.HandleFunc("POST /summarize", srv.handleSummarize)
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))

	addr := fmt.Sprintf(":%d", cfg.httpPort)
	log.Printf("🤖 Miniflux AI Dashboard starting on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
