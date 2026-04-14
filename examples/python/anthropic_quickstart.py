"""Quickstart: call Anthropic (Claude) through a local Rein proxy.

Start Rein first (from the repo root):

    cp .env.example .env
    # Edit .env and fill in REIN_ADMIN_TOKEN and REIN_ENCRYPTION_KEY.
    # Generate both with: openssl rand -hex 32
    docker compose up -d

Then create a virtual key via the admin API (see docs/quickstart.md) and
export the returned `rein_live_...` key:

    export REIN_KEY=rein_live_...
    python examples/python/anthropic_quickstart.py
"""

import os

from anthropic import Anthropic

client = Anthropic(
    api_key=os.environ["REIN_KEY"],
    base_url=os.environ.get("REIN_BASE_URL", "http://localhost:8080/v1"),
)

resp = client.messages.create(
    model="claude-sonnet-4-5-20250514",
    max_tokens=256,
    messages=[
        {"role": "user", "content": "In one sentence, what is rein?"},
    ],
)

print(resp.content[0].text)
