package elevenlabs

import (
	"encoding/json"
	"fmt"
	"strings"
)

// GetRealtimeURL builds the ConvAI websocket URL (no signed URL needed)
func GetRealtimeURL(agentID, apiKey string) string {
	return fmt.Sprintf(
		"wss://api.elevenlabs.io/v1/convai/conversation?agent_id=%s&api_key=%s",
		agentID,
		apiKey,
	)
}

// GenerateElevenLabsConfig creates configuration for initializing ElevenLabs conversation
// IMPORTANT: input_audio_format matches Twilio (mulaw_8000)
//            output_audio_format is pcm_16000 which we transcode to mulaw_8000 before sending to Twilio.
//
// firstNameOverride comes from your outbound flow (e.g. Carrd/Make).
// If it's non-empty, we prefer it over anything in userData.
func GenerateElevenLabsConfig(
	userData map[string]interface{},
	callerPhone string,
	isInbound bool,
	firstNameOverride string,
) map[string]interface{} {
	cfg := map[string]interface{}{
		"type":                "conversation_initiation_client_data",
		"input_audio_format":  "mulaw_8000",
		"output_audio_format": "pcm_16000",
		"conversation_config_override": map[string]interface{}{
			"agent": map[string]interface{}{},
		},
	}

	var firstName, lastName string

	// 1) Prefer explicit first name from the outbound payload
	if strings.TrimSpace(firstNameOverride) != "" {
		firstName = strings.TrimSpace(firstNameOverride)
	}

	// 2) Fall back to userData if we didn't get an explicit first name
	if userData != nil && firstName == "" {
		// If your user data has a different shape, adjust here
		if debtor, ok := userData["debtor"].(map[string]interface{}); ok {
			if fn, ok := debtor["first_name"].(string); ok {
				firstName = fn
			}
			if ln, ok := debtor["last_name"].(string); ok {
				lastName = ln
			}
		}
		if fn, ok := userData["first_name"].(string); ok && firstName == "" {
			firstName = fn
		}
		if ln, ok := userData["last_name"].(string); ok && lastName == "" {
			lastName = ln
		}
	}

	fullName := strings.TrimSpace(fmt.Sprintf("%s %s", firstName, lastName))
	if fullName == "" {
		fullName
