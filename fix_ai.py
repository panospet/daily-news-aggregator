import re

with open('ai/main.py', 'r') as f:
    code = f.read()

# Replace actual newlines inside f-string for articles_str
code = re.sub(
    r'articles_str \+= f"\nΆρθρο \{i\}:\nΤίτλος: \{a\[\'title\'\]\}\nΚείμενο: \{a\[\'text\'\]\[:200\]\}\n"',
    r'articles_str += f"\\nΆρθρο {i}:\\nΤίτλος: {a[\'title\']}\\nΚείμενο: {a[\'text\'][:200]}\\n"',
    code
)

with open('ai/main.py', 'w') as f:
    f.write(code)

