package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

type WebRTCServer struct {
	peerConnection    *webrtc.PeerConnection
	audioTrack        *webrtc.TrackLocalStaticSample
	mu                sync.Mutex
	isSpeaking        bool
	audioBuffer       [][]byte
	stopTrack         chan struct{}
	trackActive       bool
	stopTrackMu       sync.Mutex
	pendingCandidates []webrtc.ICECandidateInit // Buffer for ICE candidates received before PC is ready
	dataChannel       *webrtc.DataChannel
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

	var msg struct {
		SDP string `json:"sdp"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a new peer connection if one doesn't exist
	if s.peerConnection == nil {
		log.Printf("Creating new peer connection")
		config := webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
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
				{
					URLs:       []string{"turn:openrelay.metered.ca:80?transport=tcp"},
					Username:   "openrelayproject",
					Credential: "openrelayproject",
				},
			},
			ICETransportPolicy: webrtc.ICETransportPolicyAll,
			BundlePolicy:       webrtc.BundlePolicyMaxBundle,
			RTCPMuxPolicy:      webrtc.RTCPMuxPolicyRequire,
		}

		peerConnection, err := webrtc.NewPeerConnection(config)
		if err != nil {
			log.Printf("Failed to create peer connection: %v", err)
			http.Error(w, fmt.Sprintf("Failed to create peer connection: %v", err), http.StatusInternalServerError)
			return
		}

		// Log ICE connection state changes
		peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
			log.Printf("ICE Connection State changed to: %s", state)
		})

		// Log signaling state changes
		peerConnection.OnSignalingStateChange(func(state webrtc.SignalingState) {
			log.Printf("Signaling State changed to: %s", state)
		})

		// Create audio track for echo
		audioTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion")
		if err != nil {
			log.Printf("Failed to create audio track: %v", err)
			http.Error(w, fmt.Sprintf("Failed to create audio track: %v", err), http.StatusInternalServerError)
			return
		}

		s.peerConnection = peerConnection
		s.audioTrack = audioTrack

		// Add the audio track to the peer connection
		_, err = peerConnection.AddTrack(audioTrack)
		if err != nil {
			log.Printf("Failed to add audio track: %v", err)
			http.Error(w, fmt.Sprintf("Failed to add audio track: %v", err), http.StatusInternalServerError)
			return
		}

		// Handle incoming tracks
		peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			log.Printf("New track received: %s, kind: %s, codec: %s", track.ID(), track.Kind(), track.Codec().MimeType)

			// Create a buffer to store the audio data
			buf := make([]byte, 1500)

			for {
				// Read audio data from track
				i, _, err := track.Read(buf)
				if err != nil {
					log.Printf("Error reading track: %v", err)
					return
				}

				// Check if we should process this packet
				s.mu.Lock()
				shouldProcess := s.isSpeaking && s.trackActive
				s.mu.Unlock()

				if shouldProcess {
					// Get the RTP packet
					packet := make([]byte, i)
					copy(packet, buf[:i])

					// Create a new RTP packet with the same header
					rtpPacket := &rtp.Packet{}
					if err := rtpPacket.Unmarshal(packet); err != nil {
						log.Printf("Error unmarshaling RTP packet: %v", err)
						continue
					}

					s.mu.Lock()
					// Store the complete RTP packet in the buffer
					s.audioBuffer = append(s.audioBuffer, packet)
					s.mu.Unlock()
					log.Printf("Buffered audio packet: seq=%d, ts=%d, size=%d bytes",
						rtpPacket.SequenceNumber, rtpPacket.Timestamp, len(packet))
				}

				// Check if we should stop processing
				select {
				case <-s.stopTrack:
					log.Printf("Stopping track processing")
					s.mu.Lock()
					s.trackActive = false
					s.mu.Unlock()
					return
				default:
					// Continue processing
				}
			}
		})

		// Handle incoming data channel
		peerConnection.OnDataChannel(func(dc *webrtc.DataChannel) {
			log.Printf("New data channel received: %s", dc.Label())
			s.dataChannel = dc

			dc.OnOpen(func() {
				log.Printf("Data channel opened: %s", dc.Label())
			})

			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				var controlMsg struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(msg.Data, &controlMsg); err != nil {
					log.Printf("Error unmarshaling control message: %v", err)
					return
				}

				s.mu.Lock()
				switch controlMsg.Type {
				case "start_speak":
					s.isSpeaking = true
					s.trackActive = true
					log.Printf("Start speak received")
				case "end_speak":
					s.isSpeaking = false
					s.trackActive = false
					log.Printf("End speak received")

					// Send back buffered audio
					if len(s.audioBuffer) > 0 {
						log.Printf("Sending back %d buffered audio packets", len(s.audioBuffer))

						// Make a copy of the buffer before clearing it
						bufferCopy := make([][]byte, len(s.audioBuffer))
						copy(bufferCopy, s.audioBuffer)
						s.audioBuffer = make([][]byte, 0)

						// Create a goroutine to handle audio playback
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
				default:
					log.Printf("Unknown control message type: %s", controlMsg.Type)
				}
				s.mu.Unlock()
			})

			dc.OnClose(func() {
				log.Printf("Data channel closed: %s", dc.Label())
				s.mu.Lock()
				s.dataChannel = nil
				s.mu.Unlock()
			})
		})
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  msg.SDP,
	}

	log.Printf("Setting remote description (offer)")
	err := s.peerConnection.SetRemoteDescription(offer)
	if err != nil {
		log.Printf("Failed to set remote description: %v", err)
		http.Error(w, fmt.Sprintf("Failed to set remote description: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Creating answer")
	answer, err := s.peerConnection.CreateAnswer(nil)
	if err != nil {
		log.Printf("Failed to create answer: %v", err)
		http.Error(w, fmt.Sprintf("Failed to create answer: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Setting local description (answer)")
	err = s.peerConnection.SetLocalDescription(answer)
	if err != nil {
		log.Printf("Failed to set local description: %v", err)
		http.Error(w, fmt.Sprintf("Failed to set local description: %v", err), http.StatusInternalServerError)
		return
	}

	// Add any pending ICE candidates
	log.Printf("Adding %d pending ICE candidates", len(s.pendingCandidates))
	for _, candidate := range s.pendingCandidates {
		if err := s.peerConnection.AddICECandidate(candidate); err != nil {
			log.Printf("Error adding pending ICE candidate: %v", err)
		} else {
			log.Printf("Successfully added pending ICE candidate")
		}
	}
	s.pendingCandidates = nil // Clear the pending candidates

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(answer)
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

func (s *WebRTCServer) handleIceCandidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var msg struct {
		Candidate webrtc.ICECandidateInit `json:"candidate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		log.Printf("Error decoding ICE candidate: %v", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// If peer connection is not ready, store the candidate
	if s.peerConnection == nil {
		s.pendingCandidates = append(s.pendingCandidates, msg.Candidate)
		log.Printf("Stored ICE candidate for later (total pending: %d)", len(s.pendingCandidates))
		w.WriteHeader(http.StatusOK)
		return
	}

	// Add the candidate to the existing peer connection
	log.Printf("Adding ICE candidate to peer connection")
	err := s.peerConnection.AddICECandidate(msg.Candidate)
	if err != nil {
		log.Printf("Error adding ICE candidate: %v", err)
		http.Error(w, fmt.Sprintf("Failed to add ICE candidate: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully added ICE candidate")
	w.WriteHeader(http.StatusOK)
}

func main() {
	port := flag.String("port", "8080", "Port to listen on")
	flag.Parse()

	server := NewWebRTCServer()

	// Serve static files
	fs := http.FileServer(http.Dir("."))
	http.Handle("/", fs)

	// WebRTC signaling endpoints
	http.HandleFunc("/offer", server.handleOffer)
	http.HandleFunc("/answer", server.handleAnswer)
	http.HandleFunc("/ice-candidate", server.handleIceCandidate)

	log.Printf("Server starting on port %s...", *port)
	if err := http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatal(err)
	}
}
