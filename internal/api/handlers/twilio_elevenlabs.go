// -----------------------------
// MAX CALL DURATION (8 minutes)
// -----------------------------
go func(localStream string) {
	timeout := 8 * time.Minute
	log.Printf("[Guardrail] Max-duration timer started (%s) for stream %s", timeout, localStream)

	<-time.After(timeout)

	mu.Lock()
	alreadyDisconnecting := isDisconnecting
	mu.Unlock()

	if alreadyDisconnecting {
		log.Printf("[Guardrail] Timer fired but call already disconnecting (stream %s)", localStream)
		return
	}

	log.Printf("[Guardrail] Max duration reached for stream %s — disconnecting...", localStream)
	disconnectCall()
}(streamSid)
