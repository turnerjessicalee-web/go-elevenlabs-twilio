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
		return "", fmt.Errorf("error parsing signed URL response: %w", err)
	}

	return result.SignedURL, nil
}

// GenerateElevenLabsConfig creates configuration for initializing ElevenLabs conversation
func GenerateElevenLabsConfig(userData map[string]interface{}, callerPhone string, isInbound bool) map[string]interface{} {
	config := map[string]interface{}{
		"type":               "conversation_initiation_client_data",
		// IMPORTANT: Twilio <-> ElevenLabs must both use ulaw_8000 (µ-law, 8kHz)
		"input_audio_format":  "ulaw_8000",
		"output_audio_format": "ulaw_8000",
		"conversation_config_override": map[string]interface{}{
			"agent": map[string]interface{}{},
		},
	}

	var firstName, lastName string

	if userData != nil {
		if debtor, ok := userData["debtor"].(map[string]interface{}); ok {
			if fn, ok := debtor["first_name"].(string); ok {
				firstName = fn
			}
			if ln, ok := debtor["last_name"].(string); ok {
				lastName = ln
			}
		}
	}

	fullName := fmt.Sprintf("%s %s", firstName, lastName)

	agentConfig := config["conversation_config_override"].(map[string]interface{})["agent"].(map[string]interface{})

	// Set prompt based on call direction
	if isInbound {
		inboundPrompt, _ := generateInboundCallPrompt(fullName)
		agentConfig["prompt"] = map[string]interface{}{
			"prompt": inboundPrompt,
		}
	} else {
		outboundPrompt, _ := generateOutboundCallPrompt(fullName)
		agentConfig["prompt"] = map[string]interface{}{
			"prompt": outboundPrompt,
		}
	}

	// add dynamic variables if user data is available
	if userData != nil {
		config["client_data"] = map[string]interface{}{
			"dynamic_variables": map[string]string{
				"caller_phone": callerPhone,
				"caller_name":  fullName,
			},
		}
	}

	return config
}

func generateInboundCallPrompt(name string) (string, error) {
	prompt := `
You are an AI Agent that is supportive and helpful.
Your main task is to motivate the interlocutor whose name is %s to enjoy their life.
`
	return fmt.Sprintf(prompt, name), nil
}

func generateOutboundCallPrompt(name string) (string, error) {
	prompt := `
You are an AI Agent that is supportive and helpful.
Your main task is to motivate the interlocutor whose name is %s to enjoy their life.
`
	return fmt.Sprintf(prompt, name), nil
}
