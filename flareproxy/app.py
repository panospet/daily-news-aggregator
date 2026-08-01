import os
import re
import html
import requests
from flask import Flask, request, Response

app = Flask(__name__)
FLARESOLVERR_URL = os.environ.get("FLARESOLVERR_URL", "http://flaresolverr:8191/v1")

@app.route('/feed')
def fetch_feed():
    target_url = request.args.get('url')
    if not target_url:
        return "Missing 'url' query parameter", 400

    payload = {
        "cmd": "request.get",
        "url": target_url,
        "maxTimeout": 60000
    }

    try:
        # Forward request to FlareSolverr
        r = requests.post(FLARESOLVERR_URL, json=payload, timeout=70)
        res = r.json()

        if res.get('status') == 'ok':
            solution = res.get('solution', {})
            response_text = solution.get('response', '')
            
            # If the browser wrapped the XML in a <pre> block (common for XML files rendered in Chromium)
            if "<pre" in response_text:
                match = re.search(r'<pre[^>]*>(.*?)</pre>', response_text, re.DOTALL | re.IGNORECASE)
                if match:
                    response_text = match.group(1)
                response_text = html.unescape(response_text)
            
            response_text = response_text.strip()
            
            # Return solved feed XML with appropriate content-type
            return Response(response_text, mimetype='application/xml')
        else:
            return f"FlareSolverr Error: {res.get('message')}", 502

    except Exception as e:
        return f"Proxy Error: {str(e)}", 500

if __name__ == '__main__':
    app.run(host='0.0.0.0', port=8000)
