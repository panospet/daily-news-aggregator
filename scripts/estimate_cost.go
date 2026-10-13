package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"
)

type AIDashboardData struct {
	Data struct {
		Categories []struct {
			Category string `json:"category"`
			Groups   []struct {
				Articles []struct {
					Title string `json:"title"`
				} `json:"articles"`
			} `json:"groups"`
		} `json:"categories"`
	} `json:"data"`
}

func main() {
	rdb := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
	})
	ctx := context.Background()

	val, err := rdb.Get(ctx, "ai_cache").Result()
	if err != nil {
		log.Fatalf("Error reading from Redis: %v", err)
	}

	var data AIDashboardData
	err = json.Unmarshal([]byte(val), &data)
	if err != nil {
		log.Fatalf("Error unmarshaling JSON: %v", err)
	}

	prompt := "Does this headline announce a newly released song or album? Answer strictly YES or NO.\nHeadline: "

	totalInputChars := 0
	musicArticles := 0

	fmt.Println("Estimating cost for music articles...")

	for _, category := range data.Data.Categories {
		if category.Category == "Μουσικά νέα" || category.Category == "Νέες μουσικές κυκλοφορίες" {
			for _, group := range category.Groups {
				for _, article := range group.Articles {
					musicArticles++
					fullPrompt := prompt + article.Title
					totalInputChars += len(fullPrompt)
				}
			}
		}
	}
	
	totalInputTokens := totalInputChars / 4
	totalOutputTokens := musicArticles * 2

	inputCost := float64(totalInputTokens) / 1000000.0 * 0.075
	outputCost := float64(totalOutputTokens) / 1000000.0 * 0.30
	totalCost := inputCost + outputCost

	fmt.Printf("\nResults for %d music articles (last 12h batch):\n", musicArticles)
	fmt.Printf("Total Input Tokens: %d\n", totalInputTokens)
	fmt.Printf("Total Output Tokens: %d (Estimated)\n", totalOutputTokens)
	fmt.Printf("\nCost per batch (12 hours):\n")
	fmt.Printf("Input Cost:  $%.6f\n", inputCost)
	fmt.Printf("Output Cost: $%.6f\n", outputCost)
	fmt.Printf("Total Cost:  $%.6f\n", totalCost)
}
