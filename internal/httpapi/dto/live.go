package dto

import "time"

// LiveSessionBody starts a live voice session (§8.4).
type LiveSessionBody struct {
	Offer    string `json:"offer" minLength:"1" maxLength:"65536" doc:"The browser's WebRTC offer (SDP) with all its ICE candidates gathered; the backend does not trickle."`
	Takeover bool   `json:"takeover,omitempty" doc:"End a running session and start this one (for example after the panel was reloaded). Without it, a running session refuses the offer with live_session_active."`
}

// LiveSession is the answer to a live voice offer.
type LiveSession struct {
	SessionID uint64 `json:"session_id"`
	Answer    string `json:"answer" doc:"The backend's WebRTC answer (SDP) with its candidates."`
}

// LiveInput is the state of the live voice input.
type LiveInput struct {
	Available       bool       `json:"available" doc:"The bridge's UDP port is open; without it no session can start."`
	State           string     `json:"state" enum:"off,connecting,on_air,reconnecting" doc:"connecting: waiting for audio; on_air: audio reaches Liquidsoap; reconnecting: audio arrives but Liquidsoap's live input is not reachable."`
	SessionID       *uint64    `json:"session_id,omitempty"`
	OnAirSince      *time.Time `json:"on_air_since,omitempty" doc:"When audio first reached Liquidsoap in this session."`
	HarborConnected bool       `json:"harbor_connected" doc:"The bridge is connected to Liquidsoap's live input."`
	Packets         uint64     `json:"packets" doc:"Audio packets received in this session."`
	LastPacketAt    *time.Time `json:"last_packet_at,omitempty"`
}
