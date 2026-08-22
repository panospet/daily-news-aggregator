import json

with open('/tmp/ai_cache.json', 'r') as f:
    ai_cache = f.read()

data = json.loads(ai_cache)

prompt_template = "Does this headline announce a newly released song or album? Answer strictly YES or NO.\nHeadline: "

music_articles = 0
total_input_chars = 0
total_output_chars = 4 # "YES\n" or "NO\n"

for category in data.get('data', {}).get('categories', []):
    if category['category'] in ["Μουσικά νέα", "Νέες μουσικές κυκλοφορίες"]:
        for group in category.get('groups', []):
            for article in group.get('articles', []):
                music_articles += 1
                full_prompt = prompt_template + article.get('title', '')
                total_input_chars += len(full_prompt)

# A rough estimate is 4 characters per token
total_input_tokens = total_input_chars / 4
total_output_tokens = (total_output_chars / 4) * music_articles

# Gemini 2.5 Flash-Lite pricing per 1 Million tokens:
# Input: $0.075
# Output: $0.30

input_cost = (total_input_tokens / 1000000) * 0.075
output_cost = (total_output_tokens / 1000000) * 0.30
total_cost = input_cost + output_cost

print(f"Results for {music_articles} music articles (last 12h batch):")
print(f"Total Input Tokens (Est): {int(total_input_tokens)}")
print(f"Total Output Tokens (Est): {int(total_output_tokens)}")
print("")
print("Cost per batch (12 hours):")
print(f"Input Cost:  ${input_cost:.7f}")
print(f"Output Cost: ${output_cost:.7f}")
print(f"Total Cost:  ${total_cost:.7f}")
print("")
print("Estimated Monthly Cost (2 batches/day * 30 days = 60 batches):")
print(f"Monthly Cost: ${total_cost * 60:.7f}")
