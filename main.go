package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type WebRTCServer struct {
	peerConnection *webrtc.PeerConnection
	dataChannel    *webrtc.DataChannel
	audioTrack     *webrtc.TrackLocalStaticSample
	mu             sync.Mutex
	isSpeaking     bool
	audioBuffer    [][]byte
	stopTrack      chan struct{}
	trackActive    bool
	stopTrackMu    sync.Mutex
}

func testSTUNConnectivity() {
	stunServers := []string{
		"stun.l.google.com:19302",
		"stun1.l.google.com:19302",
		"stun2.l.google.com:19302",
	}

	for _, server := range stunServers {
		start := time.Now()
		conn, err := net.DialTimeout("udp", server, 5*time.Second)
		if err != nil {
			log.Printf("Failed to connect to STUN server %s: %v", server, err)
			continue
		}
		log.Printf("Successfully connected to STUN server %s in %v", server, time.Since(start))
		conn.Close()
	}
}

func (s *WebRTCServer) createPeerConnection() (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{
					"stun:stun.l.google.com:19302",
					"stun:stun1.l.google.com:19302",
				},
			},
		},
		ICETransportPolicy:   webrtc.ICETransportPolicyAll,
		ICECandidatePoolSize: 2,
		SDPSemantics:         webrtc.SDPSemanticsUnifiedPlan,
	}

	// Create media engine and setting supported codecs
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}

	// Create API with media engine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	// Create peer connection
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	// Create audio track with v4 codec parameters
	audioTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType: "audio/opus", // Simplified codec parameters
		},
		"audio",
		"pion",
	)
	if err != nil {
		return nil, err
	}

	s.audioTrack = audioTrack

	return peerConnection, nil
}

func (s *WebRTCServer) handleOffer(w http.ResponseWriter, r *http.Request) {
	// Log the raw request body for debugging
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	log.Printf("Received offer body: %s", string(body))

	// First try to decode as a raw map to check the type
	var rawOffer struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	}
	if err := json.NewDecoder(bytes.NewBuffer(body)).Decode(&rawOffer); err != nil {
		log.Printf("Error decoding raw offer: %v", err)
		http.Error(w, "Failed to decode offer", http.StatusBadRequest)
		return
	}

	// Default to "offer" if type is missing
	if rawOffer.Type == "" {
		rawOffer.Type = "offer"
		log.Printf("No type specified in offer, defaulting to 'offer'")
	}

	// Create the proper session description
	offer := webrtc.SessionDescription{
		Type: webrtc.NewSDPType(rawOffer.Type),
		SDP:  rawOffer.SDP,
	}

	// Ensure the offer has the correct SDP type
	if offer.Type != webrtc.SDPTypeOffer {
		log.Printf("Invalid SDP type received: %v", offer.Type)
		http.Error(w, "Invalid SDP type, expected 'offer'", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.peerConnection, err = s.createPeerConnection()
	s.mu.Unlock()

	if err != nil {
		log.Printf("Error creating peer connection: %v", err)
		http.Error(w, "Failed to create peer connection", http.StatusInternalServerError)
		return
	}

	// Create a channel to receive ICE gathering completion signal
	gatherComplete := webrtc.GatheringCompletePromise(s.peerConnection)

	// Add detailed ICE candidate logging
	s.peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			log.Printf("New ICE candidate: %s %s:%d typ %s",
				candidate.Protocol,
				candidate.Address,
				candidate.Port,
				candidate.Typ)
			log.Printf("Full candidate details: %v", candidate.ToJSON())
		}
	})

	s.peerConnection.OnICEGatheringStateChange(func(state webrtc.ICEGatheringState) {
		log.Printf("ICE gathering state changed to: %s", state.String())
	})

	s.peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("ICE connection state changed to: %s", state.String())
		if state == webrtc.ICEConnectionStateFailed {
			log.Printf("ICE Connection failed, checking connection stats...")
			stats := s.peerConnection.GetStats()
			log.Printf("Connection stats: %+v", stats)
		}
	})

	// OnTrack handler
	s.peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("New track received: %s, kind: %s, codec: %s", track.ID(), track.Kind(), track.Codec().MimeType)

		// Only process audio tracks
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}

		buf := make([]byte, 1500)
		for {
			n, _, err := track.Read(buf)
			if err != nil {
				log.Printf("Error reading track: %v", err)
				return
			}

			s.mu.Lock()
			shouldProcess := s.isSpeaking && s.trackActive
			s.mu.Unlock()

			if shouldProcess {
				packet := make([]byte, n)
				copy(packet, buf[:n])

				rtpPacket := &rtp.Packet{}
				if err := rtpPacket.Unmarshal(packet); err != nil {
					log.Printf("Error unmarshaling RTP packet: %v", err)
					continue
				}

				s.mu.Lock()
				s.audioBuffer = append(s.audioBuffer, packet)
				log.Printf("Buffered audio packet: seq=%d, ts=%d, size=%d bytes",
					rtpPacket.SequenceNumber, rtpPacket.Timestamp, len(packet))
				s.mu.Unlock()
			}

			select {
			case <-s.stopTrack:
				log.Printf("Stopping track processing")
				s.mu.Lock()
				s.trackActive = false
				s.mu.Unlock()
				continue
			default:
			}
		}
	})

	// Add data channel handling
	s.peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		log.Printf("New DataChannel %s %d", d.Label(), d.ID())

		s.mu.Lock()
		s.dataChannel = d
		s.mu.Unlock()

		d.OnOpen(func() {
			log.Printf("Data channel '%s'-'%d' opened.", d.Label(), d.ID())
		})

		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			log.Printf("Message from DataChannel '%s': '%s'", d.Label(), string(msg.Data))

			// Handle speak control messages
			var message map[string]interface{}
			if err := json.Unmarshal(msg.Data, &message); err == nil {
				if msgType, ok := message["type"].(string); ok {
					switch msgType {
					case "start_speak":
						s.mu.Lock()
						s.isSpeaking = true
						s.trackActive = true
						s.audioBuffer = make([][]byte, 0) // Clear previous buffer
						s.mu.Unlock()

						// Safely create new stop channel
						s.stopTrackMu.Lock()
						// Create new channel without closing the old one
						s.stopTrack = make(chan struct{})
						s.stopTrackMu.Unlock()

						log.Printf("Started speaking - ready to receive audio")
						d.SendText("Started speaking")

					case "end_speak":
						s.mu.Lock()
						s.isSpeaking = false
						s.trackActive = false
						s.mu.Unlock()

						// Safely close the stop channel
						s.stopTrackMu.Lock()
						if s.stopTrack != nil {
							select {
							case <-s.stopTrack:
								// Channel already closed
							default:
								close(s.stopTrack)
							}
						}
						s.stopTrackMu.Unlock()

						// Make a copy of the buffer before clearing it
						s.mu.Lock()
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
						d.SendText("Ended speaking")

					default:
						// Echo regular messages back
						d.SendText("Server received: " + string(msg.Data))
					}
					return
				}
			}

			// Echo regular messages back
			d.SendText("Server received: " + string(msg.Data))
		})
	})

	if err := s.peerConnection.SetRemoteDescription(offer); err != nil {
		log.Printf("Error setting remote description: %v", err)
		http.Error(w, "Failed to set remote description", http.StatusInternalServerError)
		return
	}

	answer, err := s.peerConnection.CreateAnswer(nil)
	if err != nil {
		log.Printf("Error creating answer: %v", err)
		http.Error(w, "Failed to create answer", http.StatusInternalServerError)
		return
	}

	if err := s.peerConnection.SetLocalDescription(answer); err != nil {
		log.Printf("Error setting local description: %v", err)
		http.Error(w, "Failed to set local description", http.StatusInternalServerError)
		return
	}

	// Wait for ICE gathering to complete or timeout
	select {
	case <-gatherComplete:
		log.Printf("ICE gathering completed")
	case <-time.After(3 * time.Second):
		log.Printf("ICE gathering timed out, sending partial candidates")
	}

	// Get the updated local description after ICE gathering
	answer = *s.peerConnection.LocalDescription()

	w.Header().Set("Content-Type", "application/json")
	response := struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	}{
		Type: answer.Type.String(),
		SDP:  answer.SDP,
	}
	json.NewEncoder(w).Encode(response)
}

type ICECandidate struct {
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdpMid"`
	SDPMLineIndex int    `json:"sdpMLineIndex"`
}

type ICECandidateRequest struct {
	Candidates []ICECandidate `json:"candidates"`
}

func (s *WebRTCServer) handleICECandidate(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.peerConnection == nil || s.peerConnection.RemoteDescription() == nil {
		log.Printf("Cannot process ICE candidates: peer connection not ready")
		http.Error(w, "Peer connection not ready", http.StatusConflict)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

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
		var altData struct {
			Candidates []string `json:"candidates"`
		}
		if err := json.NewDecoder(bytes.NewBuffer(body)).Decode(&altData); err != nil {
			log.Printf("Error decoding ICE candidates in both formats: %v", err)
			http.Error(w, "Failed to decode ICE candidates", http.StatusBadRequest)
			return
		}

		for _, candidateStr := range altData.Candidates {
			candidateStr = strings.TrimPrefix(candidateStr, "a=")
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

func main() {
	server := &WebRTCServer{}

	// Test STUN connectivity on startup
	testSTUNConnectivity()

	// Serve static files from the current directory
	fs := http.FileServer(http.Dir("."))
	http.Handle("/", fs)

	// WebRTC endpoints
	http.HandleFunc("/offer", server.handleOffer)
	http.HandleFunc("/ice-candidates", server.handleICECandidate)

	log.Printf("Server starting on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}
