import requests
import json
import time

def fetch_articles():
    # We can just read them from DB or fetch from Go
    pass

if __name__ == "__main__":
    t0 = time.time()
    resp = requests.get("http://localhost:8080/") # Will trigger fetchFresh internally if cache is cleared
    print(f"Time: {time.time() - t0:.2f}s")
