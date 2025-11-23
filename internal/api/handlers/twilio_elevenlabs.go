// -----------------------------------------------------------------------------
// OUTBOUND CALL HANDLERS
// -----------------------------------------------------------------------------

func HandleOutboundCall(cfg *config.Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Number    string `json:"number"`
			FirstName string `json:"first_name"`
			Email     string `json:"email"`
			Prompt    string `json:"prompt"` // optional, kept for compatibility
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if req.Number == "" {
			http.Error(w, "Phone number is required", http.StatusBadRequest)
			return
		}

		// Pass number + name + email into the TwiML endpoint
		callURL := fmt.Sprintf(
			"https://%s/outbound-call-twiml?number=%s&first_name=%s&email=%s&prompt=%s",
			r.Host,
			url.QueryEscape(req.Number),
			url.QueryEscape(req.FirstName),
			url.QueryEscape(req.Email),
			url.QueryEscape(req.Prompt),
		)

		params := map[string]string{
			"To":   req.Number,
			"From": cfg.TwilioPhoneNumber,
			"Url":  callURL,
		}

		call, err := createTwilioCall(params, cfg.TwilioAccountSID, cfg.TwilioAuthToken)
		if err != nil {
			log.Printf("[Twilio] Error creating call: %v", err)
			http.Error(w, "Failed to initiate call", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Call initiated",
			"callSid": call["sid"],
		})
	})
}

func HandleOutboundCallTwiml() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prompt := r.URL.Query().Get("prompt")
		number := r.URL.Query().Get("number")
		firstName := r.URL.Query().Get("first_name")
		email := r.URL.Query().Get("email")

		// Build user_data JSON from query params
		userData := map[string]interface{}{}
		if firstName != "" {
			userData["first_name"] = firstName
		}
		if email != "" {
			userData["email"] = email
		}
		userDataJSON, _ := json.Marshal(userData)
		escapedUserData := url.QueryEscape(string(userDataJSON))

		twiml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<Response>
    <Connect>
        <Stream url="wss://%s/media-stream">
            <Parameter name="caller_phone" value="%s" />
            <Parameter name="user_data" value="%s" />
            <Parameter name="prompt" value="%s" />
            <Parameter name="direction" value="outbound" />
        </Stream>
    </Connect>
</Response>`,
			r.Host,
			number,
			escapedUserData,
			url.QueryEscape(prompt),
		)

		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(twiml))
	})
}
