package elevenlabs

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

// GetSignedElevenLabsURL retrieves a signed WebSocket URL from ElevenLabs
func GetSignedElevenLabsURL(agentID string, apiKey string) (string, error) {
	url := fmt.Sprintf("https://api.elevenlabs.io/v1/convai/conversation/get_signed_url?agent_id=%s", agentID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("error creating signed URL request: %w", err)
	}

	req.Header.Set("xi-api-key", apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("error getting signed URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		log.Printf(
			"[ElevenLabs] failed to get signed URL: status=%s body=%s",
			resp.Status,
			string(body),
		)

		return "", fmt.Errorf("failed to get signed URL: %s", resp.Status)
	}

	var result struct {
		SignedURL string `json:"signed_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("error parsing signed URL response: %w", er
