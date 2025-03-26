package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
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

// Define the structure for incoming ICE candidates
type ICECandidate struct {
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdpMid"`
	SDPMLineIndex int    `json:"sdpMLineIndex"`
}

type ICECandidateRequest struct {
	Candidates []ICECandidate `json:"candidates"`
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

	// Add CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	// Handle preflight requests
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	var msg struct {
		SDP string `json:"sdp"`
	}

	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		log.Printf("Failed to decode offer: %v", err)
		http.Error(w, "Failed to decode offer", http.StatusBadRequest)
		return
	}

	// Log the received offer for debugging
	log.Printf("Received offer: %s", msg.SDP)

	// Create SessionDescription from SDP
	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  msg.SDP,
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
		ICEServers:           iceServers,
		ICETransportPolicy:   webrtc.ICETransportPolicyAll,
		BundlePolicy:         webrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy:        webrtc.RTCPMuxPolicyRequire,
		SDPSemantics:         webrtc.SDPSemanticsUnifiedPlan,
		ICECandidatePoolSize: 10,
	}

	// Log ICE server configuration
	log.Printf("ICE server configuration:")
	for i, server := range iceServers {
		log.Printf("Server %d: URLs=%v, Username=%s, Credential=%s",
			i, server.URLs, server.Username, server.Credential)
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

			// Log TURN server status
			for _, server := range iceServers {
				for _, url := range server.URLs {
					if strings.Contains(url, "turn") {
						log.Printf("TURN server status - URL: %s, Username: %s, Credential: %s",
							url, server.Username, server.Credential)
					}
				}
			}
		}
	})

	// Add ICE connection state change handler
	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("ICE connection state changed to: %s", state)
		if state == webrtc.ICEConnectionStateChecking {
			log.Printf("ICE connection checking - attempting to establish connection")
			// Log current candidates
			s.candidatesMutex.Lock()
			log.Printf("Current pending candidates: %d", len(s.pendingCandidates))
			s.candidatesMutex.Unlock()
		} else if state == webrtc.ICEConnectionStateFailed {
			log.Printf("ICE connection failed - checking TURN server authentication")
			// Log TURN server status
			for _, server := range iceServers {
				for _, url := range server.URLs {
					if strings.Contains(url, "turn") {
						log.Printf("TURN server authentication test - URL: %s, Username: %s, Credential: %s",
							url, server.Username, server.Credential)
						// Try to connect to TURN server to verify connectivity
						go func(turnURL, username, credential string) {
							// Extract host and port from URL, removing query parameters
							urlWithoutQuery := strings.Split(turnURL, "?")[0]
							parts := strings.Split(strings.TrimPrefix(urlWithoutQuery, "turn:"), ":")
							if len(parts) != 2 {
								log.Printf("Invalid TURN URL format: %s", turnURL)
								return
							}
							host := parts[0]
							port := parts[1]

							// Try to establish a TCP connection
							conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%s", host, port), 5*time.Second)
							if err != nil {
								log.Printf("Failed to connect to TURN server %s: %v", turnURL, err)
								return
							}
							conn.Close()
							log.Printf("Successfully connected to TURN server %s", turnURL)
						}(url, server.Username, server.Credential.(string))
					}
				}
			}
			// Log current connection state
			log.Printf("Current connection state: %s", peerConnection.ConnectionState())
			log.Printf("Current ICE gathering state: %s", peerConnection.ICEGatheringState())
		}
	})

	// Update the OnICEGatheringStateChange handler
	peerConnection.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		log.Printf("ICE gathering state changed to: %s", state.String())
		if state == webrtc.ICEGatheringStateComplete {
			log.Printf("ICE gathering completed with state: %s", state.String())

			// Get local description
			localDesc := peerConnection.LocalDescription()
			if localDesc != nil {
				log.Printf("Local description: %s", localDesc.SDP)
			}
		}
	})

	// Set up ICE candidate handler BEFORE setting remote description
	log.Printf("Setting up ICE candidate handler")
	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			candidateStr := candidate.String()
			log.Printf("New ICE candidate: %s", candidateStr)

			// Log candidate details directly
			log.Printf("Candidate details - Protocol: %s, Address: %s, Port: %d, Component: %d, Foundation: %s",
				candidate.Protocol,
				candidate.Address,
				candidate.Port,
				candidate.Component,
				candidate.Foundation)

			// Check if it's a TURN/relay candidate by looking at the candidate string
			if strings.Contains(candidateStr, "typ relay") {
				log.Printf("Found TURN candidate: %s", candidateStr)
			}
		}
	})

	// Set up connection state handler BEFORE setting remote description
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

	// Set up ICE connection state handler BEFORE setting remote description
	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("ICE connection state changed to: %s", state)
		if state == webrtc.ICEConnectionStateChecking {
			log.Printf("ICE connection checking - attempting to establish connection")
			// Log current candidates
			s.candidatesMutex.Lock()
			log.Printf("Current pending candidates: %d", len(s.pendingCandidates))
			s.candidatesMutex.Unlock()
		} else if state == webrtc.ICEConnectionStateFailed {
			log.Printf("ICE connection failed - checking TURN server authentication")
			// Log current connection state
			log.Printf("Current connection state: %s", peerConnection.ConnectionState())
			log.Printf("Current ICE gathering state: %s", peerConnection.ICEGatheringState())
		}
	})

	// Set up ICE gathering state handler BEFORE setting remote description
	peerConnection.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		log.Printf("ICE gathering state changed to: %s", state.String())
		if state == webrtc.ICEGatheringStateComplete {
			log.Printf("ICE gathering completed with state: %s", state.String())
			log.Printf("Current ICE connection state: %s", peerConnection.ICEConnectionState())
		}
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
			// Send a test message to verify the channel is working
			if err := d.SendText("Data channel ready"); err != nil {
				log.Printf("Error sending test message: %v", err)
			} else {
				log.Printf("Sent test message successfully")
			}
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body))
	log.Printf("Received ICE candidate request body: %s", string(body))

	var data ICECandidateRequest
	if err := json.NewDecoder(bytes.NewBuffer(body)).Decode(&data); err != nil {
		// Try alternative format (array of strings)
		var altData struct {
			Candidates []string `json:"candidates"`
		}
		if err := json.NewDecoder(bytes.NewBuffer(body)).Decode(&altData); err != nil {
			log.Printf("Error decoding ICE candidates in both formats: %v", err)
			http.Error(w, "Failed to decode ICE candidates", http.StatusBadRequest)
			return
		}

		// Convert string candidates to ICECandidate format
		for _, candidateStr := range altData.Candidates {
			// Remove "a=" prefix if present
			candidateStr = strings.TrimPrefix(candidateStr, "a=")
			// Remove trailing \r if present
			candidateStr = strings.TrimSuffix(candidateStr, "\r")

			sdpMid := "0"
			sdpMLineIndex := uint16(0)
			candidate := webrtc.ICECandidateInit{
				Candidate:     candidateStr,
				SDPMid:        &sdpMid,
				SDPMLineIndex: &sdpMLineIndex,
			}

			log.Printf("Processing ICE candidate from string: %s", candidateStr)
			if err := s.peerConnection.AddICECandidate(candidate); err != nil {
				log.Printf("Error adding ICE candidate: %v", err)
				continue
			}
			log.Printf("Successfully added ICE candidate from string")
		}
	} else {
		// Process regular format
		for _, candidateData := range data.Candidates {
			sdpMid := candidateData.SDPMid
			sdpMLineIndex := uint16(candidateData.SDPMLineIndex)
			candidate := webrtc.ICECandidateInit{
				Candidate:     candidateData.Candidate,
				SDPMid:        &sdpMid,
				SDPMLineIndex: &sdpMLineIndex,
			}

			log.Printf("Processing ICE candidate: %s", candidate.Candidate)
			if err := s.peerConnection.AddICECandidate(candidate); err != nil {
				log.Printf("Error adding ICE candidate: %v", err)
				continue
			}
			log.Printf("Successfully added ICE candidate")
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
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

// Add this function to load Twilio credentials
func loadTwilioCredentials() (*TwilioResponse, error) {
	accountSid := os.Getenv("TWILIO_ACCOUNT_SID")
	authToken := os.Getenv("TWILIO_AUTH_TOKEN")

	log.Printf("Checking Twilio credentials - Account SID: %s, Auth Token: %s",
		accountSid, authToken)

	if accountSid == "" || authToken == "" {
		log.Printf("Warning: TWILIO_ACCOUNT_SID or TWILIO_AUTH_TOKEN not set, using openrelay.metered.ca servers")
		// Return default configuration using openrelay.metered.ca
		return &TwilioResponse{
			ICEServers: []ICEServer{
				{
					URLs: "stun:stun.l.google.com:19302",
				},
				{
					URLs:       "turn:openrelay.metered.ca:80",
					Username:   "openrelayproject",
					Credential: "openrelayproject",
				},
				{
					URLs:       "turn:openrelay.metered.ca:443",
					Username:   "openrelayproject",
					Credential: "openrelayproject",
				},
				{
					URLs:       "turn:openrelay.metered.ca:443?transport=tcp",
					Username:   "openrelayproject",
					Credential: "openrelayproject",
				},
			},
		}, nil
	}

	// Fetch credentials from Twilio
	url := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Tokens.json", accountSid)
	log.Printf("Fetching Twilio credentials from: %s", url)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %v", err)
	}

	req.SetBasicAuth(accountSid, authToken)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error fetching Twilio credentials: %v", err)
	}
	defer resp.Body.Close()

	log.Printf("Twilio API response status: %d", resp.StatusCode)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("Twilio API returned error status: %d", resp.StatusCode)
	}

	var twilioResp TwilioResponse
	if err := json.NewDecoder(resp.Body).Decode(&twilioResp); err != nil {
		return nil, fmt.Errorf("error decoding Twilio response: %v", err)
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

	log.Printf("Successfully fetched Twilio credentials with %d ICE servers", len(twilioResp.ICEServers))
	// Log each ICE server
	for i, server := range twilioResp.ICEServers {
		log.Printf("ICE Server %d: URL=%s, Username=%s", i, server.URL, server.Username)
	}

	return &twilioResp, nil
}

// Then modify main() to use this function
func main() {
	server := &WebRTCServer{
		credentialsMutex: sync.RWMutex{},
	}

	// Load Twilio credentials
	twilioResp, err := loadTwilioCredentials()
	if err != nil {
		log.Printf("Error loading Twilio credentials: %v", err)
		// Continue with default configuration
	} else {
		server.credentialsMutex.Lock()
		server.twilioCredentials = twilioResp
		server.credentialsMutex.Unlock()
	}

	// Serve static files
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "index.html")
			return
		}
		http.NotFound(w, r)
	})

	// Set up routes
	http.HandleFunc("/ice-servers", server.getICEServers)
	http.HandleFunc("/offer", server.handleOffer)
	http.HandleFunc("/ice-candidates", server.handleICECandidate)

	log.Printf("Server starting on port 8080...")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}
