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
		if em, ok := userData["email"].(string); ok {
			email = em
		}
	}

	fullName := fmt.Sprintf("%s %s", firstName, lastName)

	agentCfg := cfg["conversation_config_override"].(map[string]interface{})["agent"].(map[string]interface{})

	// Keep underlying prompt logic (general behaviour instructions)
	var prompt string
	if isInbound {
		prompt, _ = generateInboundCallPrompt(fullName)
	} else {
		prompt, _ = generateOutboundCallPrompt(fullName)
	}

	agentCfg["prompt"] = map[string]interface{}{
		"prompt": prompt,
	}

	// --- New: First spoken message (name-aware) ---
	var firstMessage string
	if firstName != "" {
		firstMessage = fmt.Sprintf(
			"Hi %s, it's Bellkeeper, your virtual receptionist. "+
				"This is a quick live demo so you can experience how I can handle guest bookings. "+
				"When you're ready, just say: 'I'd like to make a booking.'",
			firstName,
		)
	} else {
		firstMessage = "Hi, it's Bellkeeper, your virtual receptionist. " +
			"This is a quick live demo so you can experience how I can handle guest bookings. " +
			"When you're ready, just say: 'I'd like to make a booking.'"
	}
	agentCfg["first_message"] = firstMessage

	// Pass client data if available
	if userData != nil {
		dynamic := map[string]string{
			"caller_phone": callerPhone,
			"caller_name":  fullName,
		}
		if email != "" {
			dynamic["caller_email"] = email
		}

		cfg["client_data"] = map[string]interface{}{
			"dynamic_variables": dynamic,
			"user_json":         mustJSON(userData),
		}
	}

	return cfg
}

func generateInboundCallPrompt(name string) (string, error) {
	prompt := `
You are an AI Agent that is supportive and helpful.
Your main task is to motivate the interlocutor whose name is %s to enjoy their life.
Speak clearly, like a friendly human receptionist on a phone line.
Pause to let them reply, and respond to what they actually say.
`
	return fmt.Sprintf(prompt, name), nil
}

func generateOutboundCallPrompt(name string) (string, error) {
	prompt := `
You are an AI Agent that is supportive and helpful.
You are calling %s on the phone.
Speak clearly like a human, wait for their responses, and answer conversationally.
Do not speak over the top of the other person.
`
	return fmt.Sprintf(prompt, name), nil
}

// helper to safely JSON encode into string
func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
