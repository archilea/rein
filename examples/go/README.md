# Go Examples

This directory contains standalone Go examples for using Rein.

## Available Examples

### Anthropic Quickstart

This example demonstrates how to send messages using an Anthropic virtual key, utilizing only the Go standard library (`net/http`, `encoding/json`, `os`). It uses a Claude model from `internal/meter/pricing.json`.

**Prerequisites:**
You will need a Rein instance running and an Anthropic virtual key.

**Run the example:**

```bash
# Set your environment variables
export REIN_URL=http://localhost:8080
export REIN_KEY=rein_live_YOUR_VIRTUAL_KEY

# Run the script
go run ./anthropic_quickstart.go
```
