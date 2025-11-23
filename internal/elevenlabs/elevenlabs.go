package elevenlabs

import (
	"encoding/json"
	"fmt"
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
func GenerateElevenLabsConfig(userData map[string]interface{}, callerPhone string, isInbound bool) map[string]interface{} {
	cfg := map[string]interface{}{
		"type":                "conversation_initiation_client_data",
		"input_audio_format":  "mulaw_8000",
		"output_audio_format": "pcm_16000",
		"conversation_config_override": map[string]interface{}{
			"agent": map[string]interface{}{},
		},
	}

	var firstName, lastName, email string

	if userData != nil {
		// Optional nested debtor object for flexibility
		if debtor, ok := userData["debtor"].(map[string]interface{}); ok {
			if fn, ok := debtor["first_name"].(string); ok {
				firstName = fn
			}
			if ln, ok := debtor["last_name"].(string); ok {
				lastName = ln
			}
		}

		// Flat fields
		if fn, ok := userData["first_name"].(string); ok && firstName == "" {
			firstName = fn
		}
		if ln, ok := userData["last_name"].(string); ok && lastName == "" {
			lastName = ln
		}
		if em, ok := userData["email"].(string); ok {
			email = em
		}
	}

	// Build a decent display name
	fullName := firstName
	if lastName != "" {
		if fullName != "" {
			fullName = fmt.Sprintf("%s %s", firstName, lastName)
		} else {
			fullName = lastName
		}
	}

	agentCfg := cfg["conversation_config_override"].(map[string]interface{})["agent"].(map[string]interface{})

	// ------------------------------------------------
	// FIRST MESSAGE ONLY (do NOT override base prompt)
	// ------------------------------------------------
	// Let the ElevenLabs UI “Agent instructions / System prompt”
	// control behaviour and rules. Here we only override the
	// very first utterance on OUTBOUND calls to personalise it.
	if !isInbound {
		introName := firstName
		if introName == "" {
			introName = "there"
		}

		agentCfg["first_message"] = fmt.Sprintf(
			"Hi %s, it’s Bellkeeper, your virtual receptionist. "+
				"This is a quick live demo so you can experience how I can handle guest bookings. "+
				"When you’re ready, just say: 'I’d like to make a booking.'",
			introName,
		)
	}

	// ------------------------------------------------
	// Client data / dynamic variables
	// ------------------------------------------------
	if userData != nil {
		dynamicVars := map[string]string{
			"caller_phone": callerPhone,
			"caller_name":  fullName,
		}
		if email != "" {
			dynamicVars["caller_email"] = email
		}

		cfg["client_data"] = map[string]interface{}{
			"dynamic_variables": dynamicVars,
			"user_json":         mustJSON(userData),
		}
	}

	return cfg
}

// helper to safely JSON encode into string
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
