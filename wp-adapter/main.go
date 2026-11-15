package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
	_ "time/tzdata"
)

// WPPost represents a WordPress post from the JSON API (standard WP or custom 24MEDIA).
type WPPost struct {
	// Standard WordPress REST fields (e.g. News 24/7)
	ID      int    `json:"id"`
	DateGMT string `json:"date_gmt"`
	Link    string `json:"link"`
	Title   struct {
		Rendered string `json:"rendered"`
	} `json:"title"`
	Excerpt struct {
		Rendered string `json:"rendered"`
	} `json:"excerpt"`

	// Custom 24MEDIA infinite posts fields (e.g. Sport24)
	CustomID  int    `json:"ID"`
	PostTitle string `json:"post_title"`
	PostURL   string `json:"post_url"`
	PostLead  string `json:"post_lead"`
	PostDate  string `json:"post_date"`
}

// RSS structures
type RSS struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Channel RSSChannel `xml:"channel"`
}

type RSSChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Items       []RSSItem `xml:"item"`
}

type CDATA struct {
	Text string `xml:",cdata"`
}

type RSSItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description CDATA  `xml:"description"`
	PubDate     string `xml:"pubDate"`
	GUID        string `xml:"guid"`
}

var athensLoc *time.Location

func init() {
	var err error
	athensLoc, err = time.LoadLocation("Europe/Athens")
	if err != nil {
		athensLoc = time.FixedZone("EET", 2*60*60)
	}
}

func fetchWP(w http.ResponseWriter, r *http.Request) {
	targetURL := r.URL.Query().Get("url")
	if targetURL == "" {
		targetURL = "https://www.news247.gr/wp-json/wp/v2/posts?per_page=100"
	}

	// Forward per_page if supplied as query param
	if perPage := r.URL.Query().Get("per_page"); perPage != "" {
		if parsed, err := url.Parse(targetURL); err == nil {
			q := parsed.Query()
			q.Set("per_page", perPage)
			parsed.RawQuery = q.Encode()
			targetURL = parsed.String()
		}
	}

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (WP-RSS-Adapter/1.0)")

	// Allow callers to name the feed; defaults to News 24/7.
	feedTitle := r.URL.Query().Get("title")
	if feedTitle == "" {
		feedTitle = "News 24/7 (via Adapter)"
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Failed to fetch WP JSON", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("WP API returned status: %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Failed to read response body", http.StatusInternalServerError)
		return
	}

	var posts []WPPost
	if err := json.Unmarshal(body, &posts); err != nil {
		http.Error(w, "Failed to parse JSON", http.StatusInternalServerError)
		return
	}

	rss := RSS{
		Version: "2.0",
		Channel: RSSChannel{
			Title:       feedTitle,
			Link:        targetURL,
			Description: "RSS feed generated from WP REST API",
		},
	}

	for _, p := range posts {
		title := html.UnescapeString(p.Title.Rendered)
		if title == "" {
			title = html.UnescapeString(p.PostTitle)
		}

		link := p.Link
		if link == "" {
			link = p.PostURL
		}

		desc := p.Excerpt.Rendered
		if desc == "" {
			desc = p.PostLead
		}

		item := RSSItem{
			Title:       title,
			Link:        link,
			Description: CDATA{Text: desc},
			GUID:        link,
		}

		if p.DateGMT != "" {
			t, err := time.Parse("2006-01-02T15:04:05", p.DateGMT)
			if err == nil {
				item.PubDate = t.Format(time.RFC1123Z)
			}
		} else if p.PostDate != "" {
			t, err := time.ParseInLocation("02.01.2006, 15:04", p.PostDate, athensLoc)
			if err == nil {
				item.PubDate = t.Format(time.RFC1123Z)
			}
		}

		rss.Channel.Items = append(rss.Channel.Items, item)
	}

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(rss); err != nil {
		log.Printf("Error encoding RSS: %v", err)
	}
}

func main() {
	http.HandleFunc("/feed", fetchWP)
	port := "8000"
	log.Printf("WP RSS Adapter listening on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
