// This example demonstrates how to call the Anthropic API via Rein
// using only the Go standard library.
//
// Prerequisites:
// 1. Start Rein locally:
//    go run main.go serve
// 2. Mint an Anthropic virtual key via the Admin API:
//    curl -X POST http://localhost:8080/admin/v1/keys \
//      -H 'Authorization: Bearer my-admin-key' \
//      -d '{"upstream": "anthropic"}'
// 3. Set the variables locally:
//    export REIN_URL=http://localhost:8080
//    export REIN_KEY=rein_live_...
//
// For more details see: docs/quickstart.md
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	reinURL := os.Getenv("REIN_URL")
	if reinURL == "" {
		reinURL = "http://localhost:8080"
	}

	reinKey := os.Getenv("REIN_KEY")
	if reinKey == "" {
		fmt.Println("Error: REIN_KEY environment variable is required.")
		os.Exit(1)
	}

	payload := map[string]interface{}{
		"model": "claude-sonnet-4-5",
		"messages": []map[string]interface{}{
			{
				"role":    "user",
				"content": "Hello from Go! What is 2 + 2?",
			},
		},
		"max_tokens": 1024,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Printf("Error marshaling payload: %v\n", err)
		os.Exit(1)
	}

	req, err := http.NewRequest("POST", reinURL+"/v1/messages", bytes.NewBuffer(body))
	if err != nil {
		fmt.Printf("Error creating request: %v\n", err)
		os.Exit(1)
	}

	req.Header.Set("Authorization", "Bearer "+reinKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error making request: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Printf("Error reading response: %v\n", err)
		os.Exit(1)
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("API Error (status %d): %s\n", resp.StatusCode, string(respBody))
		os.Exit(1)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		fmt.Printf("Error parsing response: %v\n\nRaw response: %s\n", err, string(respBody))
		os.Exit(1)
	}

	if contentArr, ok := result["content"].([]interface{}); ok && len(contentArr) > 0 {
		if contentMap, ok := contentArr[0].(map[string]interface{}); ok {
			if text, ok := contentMap["text"].(string); ok {
				fmt.Printf("Response: %s\n", text)
				return
			}
		}
	}

	// Fallback if the expected structure isn't there
	fmt.Printf("Parsed Response: %+v\n", result)
}
