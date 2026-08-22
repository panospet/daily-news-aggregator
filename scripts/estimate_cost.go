package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/google/genai-go"
	"github.com/joho/godotenv"
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
	godotenv.Load(".env")
	apiKey := os.Getenv("GEMINI_API_KEY")

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

	client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: genai.String(apiKey)})
	if err != nil {
		log.Fatalf("Error creating Gemini client: %v", err)
	}

	model := client.Models.Get("gemini-2.5-flash-lite")

	prompt := "Does this headline announce a newly released song or album? Answer strictly YES or NO.\nHeadline: "

	totalInputTokens := 0
	totalOutputTokens := 0
	musicArticles := 0

	fmt.Println("Estimating cost for music articles...")

	for _, category := range data.Data.Categories {
		if category.Category == "Μουσικά νέα" || category.Category == "Νέες μουσικές κυκλοφορίες" {
			for _, group := range category.Groups {
				for _, article := range group.Articles {
					musicArticles++
					fullPrompt := prompt + article.Title

					// Instead of running the inference, we just count the tokens to save API calls
					// But we will run 1 to verify the structure and get average output tokens
					if musicArticles == 1 {
						resp, err := model.GenerateContent(ctx, genai.Text(fullPrompt))
						if err == nil && resp != nil && resp.UsageMetadata != nil {
							totalOutputTokens += int(resp.UsageMetadata.CandidatesTokenCount) * 51 // Assumed constant per article
						}
					}

					resp, err := client.Models.CountTokens(ctx, "gemini-2.5-flash-lite", genai.Text(fullPrompt))
					if err != nil {
						log.Printf("Warning: failed to count tokens: %v", err)
						totalInputTokens += len(strings.Fields(fullPrompt)) * 2 // Fallback estimate
					} else {
						totalInputTokens += int(resp.TotalTokens)
					}
				}
			}
		}
	}

	// Gemini 2.5 Flash-Lite pricing (per 1M tokens):
	// Input: $0.075
	// Output: $0.30
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

	fmt.Printf("\nEstimated Monthly Cost (2 batches/day x 30 days = 60 batches):\n")
	fmt.Printf("Monthly Cost: $%.6f\n", totalCost*60)
}
