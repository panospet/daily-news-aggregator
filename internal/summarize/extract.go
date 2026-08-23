package summarize

import (
	"context"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/panospet/daily-news-aggregator/internal/models"
	"golang.org/x/net/html/charset"
)

// Extractor fetches full article text for summarization.
// RSS content fields are often short teasers (~20-40 words); feeding only
// those to the LLM invites hallucination, so we opportunistically fetch the
// real page text before summarizing.
type Extractor struct {
	// Client used for all fetches. Defaults to a 10s-timeout client.
	Client *http.Client
	// MaxBytes caps how much of each page body is read (default 512 KiB).
	MaxBytes int64
	// MaxConcurrent limits parallel fetches (default 8).
	MaxConcurrent int
	// Timeout is the overall budget for enriching one group of articles.
	Timeout time.Duration
	// UserAgent sent with requests.
	UserAgent string
}

// NewExtractor returns an Extractor with production defaults.
func NewExtractor() *Extractor {
	return &Extractor{
		Client:        &http.Client{Timeout: 10 * time.Second},
		MaxBytes:      512 << 10,
		MaxConcurrent: 8,
		Timeout:       20 * time.Second,
		UserAgent:     "Mozilla/5.0 (compatible; NewsSummarizer/1.0)",
	}
}

// scriptStyleRe matches script/style/noscript blocks to drop before text extraction.
var scriptStyleRe = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)\b[^>]*>.*?</(script|style|noscript|svg|head)>`)

// tagRe matches any HTML tag.
var tagRe = regexp.MustCompile(`<[^>]*>`)

// wsRe collapses whitespace runs.
var wsRe = regexp.MustCompile(`\s+`)

// ExtractText downloads url and returns readable plain text (title, main-ish
// paragraphs). It never returns an error for fetch/parse failures — just ""
// — because summarization should proceed with whatever content exists.
func (e *Extractor) ExtractText(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", e.userAgent())
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := e.client().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var r io.Reader = resp.Body
	if e.MaxBytes > 0 {
		r = io.LimitReader(resp.Body, e.MaxBytes)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return ""
	}
	return ExtractTextFromHTML(body, resp.Header.Get("Content-Type"))
}

// ExtractTextFromHTML converts raw HTML bytes into readable plain text.
func ExtractTextFromHTML(body []byte, contentType string) string {
	// Decode to UTF-8 when the page declares another charset (Greek sites
	// occasionally serve windows-1253 / ISO-8859-7). Uses golang.org/x/net/charset
	// via a small helper; falls back to the raw bytes on any doubt.
	if decoded, ok := decodeToUTF8(body, contentType); ok {
		body = decoded
	}
	s := string(body)

	// Cut to the main content area if common markers exist — keeps noise out.
	if i := strings.Index(s, "<article"); i > 0 {
		if j := strings.LastIndex(s[i:], "</article>"); j > 0 {
			s = s[i : i+j+len("</article>")]
		}
	}

	// Drop scripts/styles, comments, then convert block ends to newlines so
	// paragraphs don't glue together.
	s = scriptStyleRe.ReplaceAllString(s, " ")
	s = regexp.MustCompile(`(?s)<!--.*?-->`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`(?i)</(p|div|li|h[1-6]|br|tr|section)>`).ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)

	// Keep lines that look like prose: drop nav crumbs, cookie bars, short junk.
	// Collapse intra-line whitespace but preserve line structure.
	var keep []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(wsRe.ReplaceAllString(ln, " "))
		if !proseLike(ln) {
			continue
		}
		keep = append(keep, ln)
	}
	text := strings.Join(keep, "\n")
	return strings.TrimSpace(text)
}

// proseLike reports whether a line looks like article prose.
func proseLike(s string) bool {
	if len(s) < 40 { // nav links, breadcrumbs, "ΔΙΑΦΗΜΙΣΗ"
		return false
	}
	letters := 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	// Require a healthy letter ratio (drops menus of URLs/symbols).
	return letters*100/len([]rune(s)) > 60
}

func (e *Extractor) client() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// extractTextCtx fetches the page with the context's deadline respected.
func (e *Extractor) extractTextCtx(ctx context.Context, rawURL string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", e.userAgent())
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := e.client().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	var r io.Reader = resp.Body
	if e.MaxBytes > 0 {
		r = io.LimitReader(resp.Body, e.MaxBytes)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return ""
	}
	return ExtractTextFromHTML(body, resp.Header.Get("Content-Type"))
}

// decodeToUTF8 converts body to UTF-8 based on Content-Type charset and
// meta-declaration sniffing, using golang.org/x/net/html/charset.
func decodeToUTF8(body []byte, contentType string) ([]byte, bool) {
	if !bytesContainNonUTF8(body) {
		return nil, false
	}
	r, err := charset.NewReader(strings.NewReader(string(body)), contentType)
	if err != nil {
		return nil, false
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, false
	}
	return out, true
}

// bytesContainNonUTF8 is a cheap validity probe: valid UTF-8 means no work.
func bytesContainNonUTF8(b []byte) bool {
	return !utf8.Valid(b)
}

func (e *Extractor) userAgent() string {
	if e.UserAgent != "" {
		return e.UserAgent
	}
	return "Mozilla/5.0 (compatible; NewsSummarizer/1.0)"
}

// EnrichGroup fetches full text for articles whose stored Content is too
// short to summarize safely, updating Article.Content in place.
// Articles that already have enough text are left untouched.
// Failures are logged and skipped — the original content remains.
func (e *Extractor) EnrichGroup(ctx context.Context, articles []models.Article) {
	const minWords = 60
	var need []int
	for i, a := range articles {
		if len(strings.Fields(a.Content)) < minWords {
			need = append(need, i)
		}
	}
	if len(need) == 0 {
		return
	}
	if e.MaxConcurrent <= 0 {
		e.MaxConcurrent = 8
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sem := make(chan struct{}, e.MaxConcurrent)
	var wg sync.WaitGroup
	for _, i := range need {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			txt := e.extractTextCtx(ctx, articles[idx].URL)
			if w := len(strings.Fields(txt)); w >= minWords {
				articles[idx].Content = txt
				log.Printf("[extract] enriched %q (+%d words)", truncTitle(articles[idx].Title), w)
			}
		}(i)
	}
	wg.Wait()
}

func truncTitle(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 60 {
		return s
	}
	return s[:57] + "..."
}
