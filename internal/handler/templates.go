package handler

import (
	"fmt"
	"html/template"
	"io/fs"
	"strings"

	"github.com/panospet/htmx-daily-news/internal/config"
	"github.com/panospet/htmx-daily-news/internal/models"
)

// TemplateSet holds the parsed templates for full-page and partial rendering.
type TemplateSet struct {
	Base    *template.Template
	Partial *template.Template
}

// LoadTemplates parses all .tmpl files from the embedded filesystem.
func LoadTemplates(embeddedFS fs.FS) (*TemplateSet, error) {
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
		"uniqueFeeds": func(articles []models.Article) []string {
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
			content, err := fs.ReadFile(embeddedFS, path)
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

	return &TemplateSet{Base: tpl, Partial: tpl}, nil
}

// FlatToAI converts flat dashboard data to AI-grouped dashboard data
// (each article becomes its own topic group).
func FlatToAI(dd *models.DashboardData) *models.AIDashboardData {
	var cats []models.AICategoryGroup
	for _, cg := range dd.Categories {
		displayName := config.CatDisplayName[cg.Category]
		if displayName == "" {
			displayName = cg.Category
		}
		acg := models.AICategoryGroup{Category: displayName, Count: cg.Count}
		for _, a := range cg.Articles {
			acg.Groups = append(acg.Groups, models.AIGroup{Topic: a.Title, Articles: []models.Article{a}, Count: 1})
		}
		cats = append(cats, acg)
	}
	return &models.AIDashboardData{Categories: cats, Total: dd.Total, Generated: dd.Generated}
}
