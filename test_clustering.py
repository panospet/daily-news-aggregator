import requests
import json

articles = [
    {"id": 1, "title": "Τσίπρας για οικονομία", "content": "Ο πρώην πρωθυπουργός μίλησε για την οικονομία και τις επενδύσεις."},
    {"id": 2, "title": "Μητσοτάκης: Νέα μέτρα", "content": "Ο πρωθυπουργός ανακοίνωσε νέα μέτρα για την οικονομία."}
]

resp = requests.post("http://127.0.0.1:8001/cluster", json={"articles": articles})
print(resp.status_code)
print(resp.json())
