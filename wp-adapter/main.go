package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// WPPost represents a WordPress post from the JSON API.
type WPPost struct {
	ID      int    `json:"id"`
	DateGMT string `json:"date_gmt"`
	Link    string `json:"link"`
	Title   struct {
		Rendered string `json:"rendered"`
	} `json:"title"`
	Excerpt struct {
		Rendered string `json:"rendered"`
	} `json:"excerpt"`
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

func fetchWP(w http.ResponseWriter, r *http.Request) {
	targetURL := r.URL.Query().Get("url")
	if targetURL == "" {
		targetURL = "https://www.news247.gr/wp-json/wp/v2/posts?per_page=100"
	}

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (WP-RSS-Adapter/1.0)")

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
			Title:       "News 24/7 (via Adapter)",
			Link:        targetURL,
			Description: "RSS feed generated from WP REST API",
		},
	}

	for _, p := range posts {
		item := RSSItem{
			Title:       p.Title.Rendered,
			Link:        p.Link,
			Description: CDATA{Text: p.Excerpt.Rendered},
			GUID:        p.Link,
		}

		if p.DateGMT != "" {
			t, err := time.Parse("2006-01-02T15:04:05", p.DateGMT)
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
