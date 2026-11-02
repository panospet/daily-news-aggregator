import re
with open("ai/main.py", "r") as f:
    code = f.read()

new_code = code.replace("""
                try:
                    data = json.loads(text)
                    if isinstance(data, dict) and data.get("topic"):
""", """
                try:
                    data = json.loads(text)
                    if isinstance(data, dict):
                        # Deepseek often wraps the requested JSON in a "response" or "text" key
                        for k in ["response", "text", "content", "answer"]:
                            if k in data and isinstance(data[k], str) and "topic" in data[k]:
                                try:
                                    inner_data = json.loads(data[k])
                                    if "topic" in inner_data:
                                        data = inner_data
                                        break
                                except:
                                    pass

                    if isinstance(data, dict) and data.get("topic"):
""")

new_code = new_code.replace("""
                        except Exception as je:
                            print(f"Gemini batch json error: {je}")
""", """
                        except Exception as je:
                            print(f"Gemini batch json error: {je}")
                    else:
                        print(f"Batch parse failed: expected dict, got {type(data)}: {data}")
""")

with open("ai/main.py", "w") as f:
    f.write(new_code)
