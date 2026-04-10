"""Quickstart: call OpenAI through a local Rein proxy.

Start Rein first (from the repo root):

    cp .env.example .env
    # Edit .env and fill in REIN_ADMIN_TOKEN and REIN_ENCRYPTION_KEY.
    # Generate both with: openssl rand -hex 32
    docker compose up -d

Then create a virtual key via the admin API (see docs/quickstart.md) and
export the returned `rein_live_...` key:

    export REIN_KEY=rein_live_...
    python examples/python/quickstart.py
"""

import os

from openai import OpenAI

client = OpenAI(
    api_key=os.environ["REIN_KEY"],
    base_url=os.environ.get("REIN_BASE_URL", "http://localhost:8080/v1"),
)

resp = client.chat.completions.create(
    model="gpt-4o-mini",
    messages=[
        {"role": "system", "content": "You are a concise assistant."},
        {"role": "user", "content": "In one sentence, what is rein?"},
    ],
)

print(resp.choices[0].message.content)
