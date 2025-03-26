package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

type WebRTCServer struct {
	peerConnection       *webrtc.PeerConnection
	audioTrack           *webrtc.TrackLocalStaticSample
	mu                   sync.Mutex
	isSpeaking           bool
	audioBuffer          [][]byte
	stopTrack            chan struct{}
	trackActive          bool
	stopTrackMu          sync.Mutex
	pendingCandidates    []webrtc.ICECandidateInit // Buffer for ICE candidates received before PC is ready
	dataChannel          *webrtc.DataChannel
	twilioCredentials    *TwilioResponse
	credentialsMutex     sync.RWMutex
	candidatesMutex      sync.Mutex
	remoteDescriptionSet bool
}

type TwilioResponse struct {
	AccountSid  string      `json:"account_sid"`
	DateCreated string      `json:"date_created"`
	DateUpdated string      `json:"date_updated"`
	ICEServers  []ICEServer `json:"ice_servers"`
	Password    string      `json:"password"`
	TTL         string      `json:"ttl"`
	Username    string      `json:"username"`
}

type ICEServer struct {
	URL        string `json:"url"`
	URLs       string `json:"urls"`
	Username   string `json:"username,omitempty"`
	Credential string `json:"credential,omitempty"`
}

func NewWebRTCServer() *WebRTCServer {
	return &WebRTCServer{
		audioBuffer:       make([][]byte, 0),
		stopTrack:         make(chan struct{}),
		trackActive:       false,
		pendingCandidates: make([]webrtc.ICECandidateInit, 0),
	}
}

func (s *WebRTCServer) handleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Log request received
	log.Printf("Received offer request")

	var offerMsg struct {
		SDP string `json:"sdp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&offerMsg); err != nil {
		log.Printf("Failed to decode offer message: %v", err)
		http.Error(w, "Failed to decode offer", http.StatusBadRequest)
		return
	}

	// Create SessionDescription from SDP
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offerMsg.SDP,
	}

	// Log offer received
	log.Printf("Decoded offer successfully")

	s.credentialsMutex.RLock()
	if s.twilioCredentials == nil {
		s.credentialsMutex.RUnlock()
		log.Printf("No ICE servers available")
		http.Error(w, "ICE servers not configured", http.StatusInternalServerError)
		return
	}
	iceServers := s.convertTwilioICEServers(s.twilioCredentials)
	s.credentialsMutex.RUnlock()

	config := webrtc.Configuration{
		ICEServers:         iceServers,
		ICETransportPolicy: webrtc.ICETransportPolicyAll,
		BundlePolicy:       webrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy:      webrtc.RTCPMuxPolicyRequire,
	}

	// Log creating peer connection
	log.Printf("Creating new peer connection")

	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log.Printf("Failed to create peer connection: %v", err)
		http.Error(w, "Failed to create peer connection", http.StatusInternalServerError)
		return
	}

	// Log peer connection created
	log.Printf("Peer connection created successfully")

	s.peerConnection = peerConnection

	// Add connection state change handler
	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Connection state changed to: %s", state)
		if state == webrtc.PeerConnectionStateFailed {
			log.Printf("Connection failed - checking ICE candidates")
			s.candidatesMutex.Lock()
			log.Printf("Number of pending candidates: %d", len(s.pendingCandidates))
			s.candidatesMutex.Unlock()

			// Log current ICE connection state
			log.Printf("Current ICE connection state: %s", peerConnection.ICEConnectionState())
			log.Printf("Current ICE gathering state: %s", peerConnection.ICEGatheringState())

			// Log connection state details
			log.Printf("Connection state details:")
			log.Printf("- Connection state: %s", peerConnection.ConnectionState())
			log.Printf("- Signaling state: %s", peerConnection.SignalingState())
			log.Printf("- ICE gathering state: %s", peerConnection.ICEGatheringState())
			log.Printf("- ICE connection state: %s", peerConnection.ICEConnectionState())
		}
	})

	// Add ICE connection state change handler
	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("ICE connection state changed to: %s", state)
		if state == webrtc.ICEConnectionStateFailed {
			log.Printf("ICE connection failed - checking TURN server connectivity")
			// Log TURN server status
			for _, server := range iceServers {
				for _, url := range server.URLs {
					if strings.Contains(url, "turn") {
						log.Printf("TURN server URL: %s, Username: %s", url, server.Username)
						// Try to connect to TURN server to verify connectivity
						go func(turnURL, username string) {
							// Extract host and port from URL
							parts := strings.Split(strings.TrimPrefix(url, "turn:"), ":")
							if len(parts) != 2 {
								log.Printf("Invalid TURN URL format: %s", url)
								return
							}
							host := parts[0]
							port := parts[1]

							// Try to establish a TCP connection
							conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%s", host, port), 5*time.Second)
							if err != nil {
								log.Printf("Failed to connect to TURN server %s: %v", url, err)
								return
							}
							conn.Close()
							log.Printf("Successfully connected to TURN server %s", url)
						}(url, server.Username)
					}
				}
			}
			// Log current connection state
			log.Printf("Current connection state: %s", peerConnection.ConnectionState())
			log.Printf("Current ICE gathering state: %s", peerConnection.ICEGatheringState())
		}
	})

	// Add ICE gathering state change handler
	peerConnection.OnICEGatheringStateChange(func(state webrtc.ICEGathererState) {
		log.Printf("ICE gathering state changed to: %s", state)
		if state == webrtc.ICEGathererStateComplete {
			// Log ICE gathering completion
			log.Printf("ICE gathering completed with state: %s", state)
			log.Printf("Current ICE connection state: %s", peerConnection.ICEConnectionState())
		}
	})

	// Add detailed ICE candidate logging
	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			log.Printf("ICE candidate gathering completed")
			return
		}
		log.Printf("New ICE candidate: %s", candidate.String())
		log.Printf("Candidate details - Protocol: %s, Address: %s, Port: %d, Type: %s",
			candidate.Protocol, candidate.Address, candidate.Port, candidate.Typ)
	})

	// Create audio track for echo
	audioTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion")
	if err != nil {
		log.Printf("Failed to create audio track: %v", err)
		http.Error(w, "Failed to create audio track", http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.audioTrack = audioTrack
	s.mu.Unlock()

	// Add the audio track to the peer connection
	_, err = peerConnection.AddTrack(audioTrack)
	if err != nil {
		log.Printf("Failed to add audio track: %v", err)
		http.Error(w, "Failed to add audio track", http.StatusInternalServerError)
		return
	}

	// Set up track handler
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("Received track: %s", track.Kind().String())

		// Create a buffer to store audio data
		buf := make([]byte, 1500)
		for {
			i, _, err := track.Read(buf)
			if err != nil {
				log.Printf("Error reading track: %v", err)
				return
			}

			// Process the audio data
			audioData := make([]byte, i)
			copy(audioData, buf[:i])

			// If we're speaking, send the audio data back
			s.mu.Lock()
			if s.isSpeaking {
				// Get the RTP packet
				packet := make([]byte, i)
				copy(packet, buf[:i])

				// Create a new RTP packet with the same header
				rtpPacket := &rtp.Packet{}
				if err := rtpPacket.Unmarshal(packet); err != nil {
					log.Printf("Error unmarshaling RTP packet: %v", err)
					continue
				}

				// Store the complete RTP packet in the buffer
				s.audioBuffer = append(s.audioBuffer, packet)
				log.Printf("Buffered audio packet: seq=%d, ts=%d, size=%d bytes",
					rtpPacket.SequenceNumber, rtpPacket.Timestamp, len(packet))
			}
			s.mu.Unlock()
		}
	})

	// Set up data channel handler BEFORE setting remote description
	log.Printf("Setting up data channel handler")
	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		log.Printf("Received data channel: %s", d.Label())
		s.dataChannel = d

		d.OnOpen(func() {
			log.Printf("Data channel opened successfully")
		})

		d.OnClose(func() {
			log.Printf("Data channel closed")
		})

		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			log.Printf("Received message on data channel: %s", string(msg.Data))

			// Parse the message
			var data struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(msg.Data, &data); err != nil {
				log.Printf("Failed to parse message: %v", err)
				return
			}

			// Handle different message types
			switch data.Type {
			case "start_speak":
				log.Printf("Received start_speak message")
				s.mu.Lock()
				s.isSpeaking = true
				s.audioBuffer = make([][]byte, 0) // Clear previous buffer
				s.mu.Unlock()
			case "end_speak":
				log.Printf("Received end_speak message")
				s.mu.Lock()
				s.isSpeaking = false
				// Make a copy of the buffer before clearing it
				bufferCopy := make([][]byte, len(s.audioBuffer))
				copy(bufferCopy, s.audioBuffer)
				s.audioBuffer = make([][]byte, 0)
				s.mu.Unlock()

				// Echo back all buffered audio
				go func(buffer [][]byte) {
					log.Printf("Starting to echo back %d audio packets", len(buffer))

					// Create a buffered channel for audio packets
					audioChan := make(chan []byte, len(buffer))

					// Send all packets to the channel
					for _, packet := range buffer {
						audioChan <- packet
					}
					close(audioChan)

					// Process packets with proper timing
					for packet := range audioChan {
						// Parse the original RTP packet
						rtpPacket := &rtp.Packet{}
						if err := rtpPacket.Unmarshal(packet); err != nil {
							log.Printf("Error unmarshaling RTP packet during playback: %v", err)
							continue
						}

						// Write the sample with proper timing
						if err := s.audioTrack.WriteSample(media.Sample{
							Data:     rtpPacket.Payload,
							Duration: time.Millisecond * 20, // Standard Opus frame duration
						}); err != nil {
							log.Printf("Error writing sample: %v", err)
						} else {
							log.Printf("Echoed buffered audio packet: seq=%d, ts=%d, size=%d bytes",
								rtpPacket.SequenceNumber, rtpPacket.Timestamp, len(rtpPacket.Payload))
						}

						// Add a small delay between packets to maintain proper timing
						time.Sleep(time.Millisecond * 20)
					}
					log.Printf("Finished echoing back audio packets")
				}(bufferCopy)
			}
		})

		d.OnError(func(err error) {
			log.Printf("Data channel error: %v", err)
		})
	})

	// Set the remote description
	log.Printf("Setting remote description")
	if err := peerConnection.SetRemoteDescription(offer); err != nil {
		log.Printf("Failed to set remote description: %v", err)
		http.Error(w, "Failed to set remote description", http.StatusInternalServerError)
		return
	}

	s.candidatesMutex.Lock()
	s.remoteDescriptionSet = true
	// Now that remote description is set, we can add any pending candidates
	log.Printf("Adding %d pending candidates", len(s.pendingCandidates))
	for _, candidate := range s.pendingCandidates {
		if err := peerConnection.AddICECandidate(candidate); err != nil {
			log.Printf("Failed to add pending candidate: %v", err)
		} else {
			log.Printf("Successfully added pending candidate")
		}
	}
	s.pendingCandidates = nil
	s.candidatesMutex.Unlock()

	// Create answer
	log.Printf("Creating answer")
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		log.Printf("Failed to create answer: %v", err)
		http.Error(w, "Failed to create answer", http.StatusInternalServerError)
		return
	}

	// Set local description
	log.Printf("Setting local description")
	if err := peerConnection.SetLocalDescription(answer); err != nil {
		log.Printf("Failed to set local description: %v", err)
		http.Error(w, "Failed to set local description", http.StatusInternalServerError)
		return
	}

	// Send the answer
	log.Printf("Sending answer")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(answer); err != nil {
		log.Printf("Failed to encode answer: %v", err)
		http.Error(w, "Failed to encode answer", http.StatusInternalServerError)
		return
	}

	log.Printf("Offer/Answer exchange completed successfully")
}

func (s *WebRTCServer) handleAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var msg struct {
		SDP string `json:"sdp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  msg.SDP,
	}

	s.mu.Lock()
	err := s.peerConnection.SetRemoteDescription(answer)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to set remote description: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *WebRTCServer) handleICECandidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Candidate webrtc.ICECandidateInit `json:"candidate"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("Failed to decode ICE candidate: %v", err)
		http.Error(w, "Failed to decode ICE candidate", http.StatusBadRequest)
		return
	}

	log.Printf("Received ICE candidate: %+v", body.Candidate)

	s.candidatesMutex.Lock()
	defer s.candidatesMutex.Unlock()

	if !s.remoteDescriptionSet {
		// Store candidate for later if remote description isn't set yet
		s.pendingCandidates = append(s.pendingCandidates, body.Candidate)
		log.Printf("Stored pending ICE candidate (total pending: %d)", len(s.pendingCandidates))
		w.WriteHeader(http.StatusOK)
		return
	}

	if s.peerConnection == nil {
		log.Printf("Peer connection is nil, cannot add ICE candidate")
		http.Error(w, "Peer connection not initialized", http.StatusInternalServerError)
		return
	}

	if err := s.peerConnection.AddICECandidate(body.Candidate); err != nil {
		log.Printf("Failed to add ICE candidate: %v", err)
		http.Error(w, "Failed to add ICE candidate", http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully added ICE candidate")
	w.WriteHeader(http.StatusOK)
}

func (s *WebRTCServer) getICEServers(w http.ResponseWriter, r *http.Request) {
	// Add debug logging
	log.Printf("Received request for ICE servers")

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.credentialsMutex.RLock()
	defer s.credentialsMutex.RUnlock()

	// Add debug logging
	if s.twilioCredentials == nil {
		log.Printf("Twilio credentials are nil")
		http.Error(w, "ICE servers not available", http.StatusInternalServerError)
		return
	}

	// Add debug logging
	log.Printf("Found %d ICE servers", len(s.twilioCredentials.ICEServers))

	response := struct {
		ICEServers []ICEServer `json:"iceServers"`
	}{
		ICEServers: s.twilioCredentials.ICEServers,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode response: %v", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully sent ICE servers response")
}

func (s *WebRTCServer) convertTwilioICEServers(twilioResp *TwilioResponse) []webrtc.ICEServer {
	if twilioResp == nil {
		// Fallback to default configuration
		log.Printf("Using default ICE servers")
		return []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
			{
				URLs:       []string{"turn:openrelay.metered.ca:80"},
				Username:   "openrelayproject",
				Credential: "openrelayproject",
			},
			{
				URLs:       []string{"turn:openrelay.metered.ca:443"},
				Username:   "openrelayproject",
				Credential: "openrelayproject",
			},
			{
				URLs:       []string{"turn:openrelay.metered.ca:443?transport=tcp"},
				Username:   "openrelayproject",
				Credential: "openrelayproject",
			},
		}
	}

	// Use a map to prevent duplicate URLs
	urlMap := make(map[string]bool)
	var iceServers []webrtc.ICEServer

	for _, server := range twilioResp.ICEServers {
		// Use URL if available, otherwise use URLs
		url := server.URL
		if url == "" {
			url = server.URLs
		}

		if url == "" {
			log.Printf("Warning: ICE server has no URL")
			continue
		}

		if urlMap[url] {
			log.Printf("Skipping duplicate URL: %s", url)
			continue
		}
		urlMap[url] = true

		iceServer := webrtc.ICEServer{
			URLs: []string{url},
		}

		if server.Username != "" && server.Credential != "" {
			iceServer.Username = server.Username
			iceServer.Credential = server.Credential
			log.Printf("TURN server configured - URL: %s, Username: %s", url, server.Username)
		}

		log.Printf("Adding ICE server: %s", url)
		iceServers = append(iceServers, iceServer)
	}

	return iceServers
}

func main() {
	server := &WebRTCServer{
		credentialsMutex: sync.RWMutex{},
	}

	// Serve static files (including index.html)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "index.html")
			return
		}
		http.NotFound(w, r)
	})

	// Fetch initial Twilio credentials
	accountSid := os.Getenv("TWILIO_ACCOUNT_SID")
	authToken := os.Getenv("TWILIO_AUTH_TOKEN")

	log.Printf("Checking Twilio credentials - Account SID: %s, Auth Token: %s",
		accountSid, authToken)

	if accountSid == "" || authToken == "" {
		log.Printf("Warning: TWILIO_ACCOUNT_SID or TWILIO_AUTH_TOKEN not set, using default credentials")
		// Set some default credentials for testing
		server.twilioCredentials = &TwilioResponse{
			ICEServers: []ICEServer{
				{
					URLs: "stun:stun.l.google.com:19302",
				},
				{
					URLs:       "turn:global.turn.twilio.com:3478?transport=udp",
					Username:   "test_user",
					Credential: "test_credential",
				},
			},
		}
		log.Printf("Using test credentials for TURN servers")
	} else {
		// Fetch credentials from Twilio
		url := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Tokens.json", accountSid)
		log.Printf("Fetching Twilio credentials from: %s", url)

		client := &http.Client{Timeout: 10 * time.Second}
		req, err := http.NewRequest("POST", url, nil)
		if err != nil {
			log.Printf("Error creating request: %v", err)
			return
		}

		req.SetBasicAuth(accountSid, authToken)
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Error fetching Twilio credentials: %v", err)
			return
		}
		defer resp.Body.Close()

		log.Printf("Twilio API response status: %d", resp.StatusCode)
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			log.Printf("Twilio API returned error status: %d", resp.StatusCode)
			return
		}

		var twilioResp TwilioResponse
		if err := json.NewDecoder(resp.Body).Decode(&twilioResp); err != nil {
			log.Printf("Error decoding Twilio response: %v", err)
			return
		}

		// Save the response to a file for debugging
		twilioRespJSON, err := json.MarshalIndent(twilioResp, "", "  ")
		if err != nil {
			log.Printf("Error marshaling Twilio response: %v", err)
		} else {
			err = os.WriteFile("twilio_response.json", twilioRespJSON, 0644)
			if err != nil {
				log.Printf("Error saving Twilio response to file: %v", err)
			} else {
				log.Printf("Saved Twilio response to twilio_response.json")
			}
		}

		server.credentialsMutex.Lock()
		server.twilioCredentials = &twilioResp
		server.credentialsMutex.Unlock()

		log.Printf("Successfully initialized Twilio credentials with %d ICE servers", len(twilioResp.ICEServers))
		// Log each ICE server
		for i, server := range twilioResp.ICEServers {
			log.Printf("ICE Server %d: URL=%s, Username=%s", i, server.URL, server.Username)
		}
	}

	// Set up routes
	http.HandleFunc("/ice-servers", server.getICEServers)
	http.HandleFunc("/offer", server.handleOffer)
	http.HandleFunc("/ice-candidate", server.handleICECandidate)

	log.Printf("Server starting on port 8080...")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}
