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
		"type":               "conversation_initiation_client_data",
		"input_audio_format": "mulaw_8000",
		"output_audio_format": "pcm_16000",
		"conversation_config_override": map[string]interface{}{
			"agent": map[string]interface{}{},
		},
	}

	var firstName, lastName string

	if userData != nil {
		// Optional nested shape: userData["debtor"].first_name / last_name
		if debtor, ok := userData["debtor"].(map[string]interface{}); ok {
			if fn, ok := debtor["first_name"].(string); ok {
				firstName = fn
			}
			if ln, ok := debtor["last_name"].(string); ok {
				lastName = ln
			}
		}
		// Flat shape: userData["first_name"], userData["last_name"]
		if fn, ok := userData["first_name"].(string); ok && firstName == "" {
			firstName = fn
		}
		if ln, ok := userData["last_name"].(string); ok && lastName == "" {
			lastName = ln
		}
	}

	fullName := fmt.Sprintf("%s %s", firstName, lastName)

	agentCfg := cfg["conversation_config_override"].(map[string]interface{})["agent"].(map[string]interface{})

	// Build the main prompt (instructions to the agent)
	var prompt string
	if isInbound {
		prompt, _ = generateInboundCallPrompt(fullName)
	} else {
		prompt, _ = generateOutboundCallPrompt(fullName)
	}
	agentCfg["prompt"] = map[string]interface{}{
		"prompt": prompt,
	}

	// Build a personalised first spoken message if we have a name
	var firstMessage string
	if firstName != "" {
		firstMessage = fmt.Sprintf(
			"Hi %s, I’m Bell, your AI Bellkeeper. I can help you with bookings or anything about your stay – what can I do for you?",
			firstName,
		)
	} else {
		firstMessage = "Hi, I’m Bell, your AI Bellkeeper. I can help you with bookings or anything about your stay – what can I do for you?"
	}
	agentCfg["first_message"] = firstMessage

	// Pass client data / dynamic variables if available
	if userData != nil {
		cfg["client_data"] = map[string]interface{}{
			"dynamic_variables": map[string]string{
				"caller_phone": callerPhone,
				"caller_name":  fullName,
			},
			"user_json": mustJSON(userData),
		}
	}

	return cfg
}

func generateInboundCallPrompt(name string) (string, error) {
	prompt := `
You are an AI Agent that is supportive, clear and helpful for motel guests.
The caller's name is %s.
You are acting as an AI Bellkeeper for a motel: you can answer questions, help with bookings, changes, and general information.
Speak clearly, like a friendly human receptionist on a phone line.
Pause often to let them reply, and respond to what they actually say.
Avoid talking over the caller.
`
	return fmt.Sprintf(prompt, name), nil
}

func generateOutboundCallPrompt(name string) (string, error) {
	prompt := `
You are an AI Agent that is supportive, clear and helpful for motel guests.
You are calling %s on the phone as their AI Bellkeeper from the motel.
You can help with bookings, confirmations, changes, and questions about their stay.
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
