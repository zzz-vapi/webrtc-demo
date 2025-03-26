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

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type WebRTCServer struct {
	peerConnection *webrtc.PeerConnection
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
	}

	return webrtc.NewPeerConnection(config)
}

func (s *WebRTCServer) handleOffer(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	log.Printf("Received offer body: %s", string(body))

	var rawOffer struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	}
	if err := json.NewDecoder(bytes.NewBuffer(body)).Decode(&rawOffer); err != nil {
		log.Printf("Error decoding raw offer: %v", err)
		http.Error(w, "Failed to decode offer", http.StatusBadRequest)
		return
	}

	if rawOffer.Type == "" {
		rawOffer.Type = "offer"
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.NewSDPType(rawOffer.Type),
		SDP:  rawOffer.SDP,
	}

	s.mu.Lock()
	s.peerConnection, err = s.createPeerConnection()
	s.mu.Unlock()

	if err != nil {
		log.Printf("Error creating peer connection: %v", err)
		http.Error(w, "Failed to create peer connection", http.StatusInternalServerError)
		return
	}

	// Create audio track for echo
	audioTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion")
	if err != nil {
		log.Printf("Failed to create audio track: %v", err)
		http.Error(w, "Failed to create audio track", http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.audioTrack = audioTrack
	s.audioBuffer = make([][]byte, 0)
	s.stopTrack = make(chan struct{})
	s.trackActive = false
	s.mu.Unlock()

	// Add the audio track to the peer connection
	_, err = s.peerConnection.AddTrack(audioTrack)
	if err != nil {
		log.Printf("Failed to add audio track: %v", err)
		http.Error(w, "Failed to add audio track", http.StatusInternalServerError)
		return
	}

	// Handle incoming tracks
	s.peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("New track received: %s, kind: %s, codec: %s", track.ID(), track.Kind(), track.Codec().MimeType)

		buf := make([]byte, 1500)
		for {
			i, _, err := track.Read(buf)
			if err != nil {
				log.Printf("Error reading track: %v", err)
				return
			}

			s.mu.Lock()
			shouldProcess := s.isSpeaking && s.trackActive
			s.mu.Unlock()

			if shouldProcess {
				packet := make([]byte, i)
				copy(packet, buf[:i])

				rtpPacket := &rtp.Packet{}
				if err := rtpPacket.Unmarshal(packet); err != nil {
					log.Printf("Error unmarshaling RTP packet: %v", err)
					continue
				}

				s.mu.Lock()
				s.audioBuffer = append(s.audioBuffer, packet)
				s.mu.Unlock()
				log.Printf("Buffered audio packet: seq=%d, ts=%d, size=%d bytes",
					rtpPacket.SequenceNumber, rtpPacket.Timestamp, len(packet))
			}

			select {
			case <-s.stopTrack:
				log.Printf("Stopping track processing")
				s.mu.Lock()
				s.trackActive = false
				s.mu.Unlock()
				continue
			default:
				// Continue processing
			}
		}
	})

	// Handle data channel
	s.peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		log.Printf("New DataChannel %s, ID: %d", d.Label(), d.ID())

		d.OnOpen(func() {
			log.Printf("DataChannel %s opened", d.Label())
		})

		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			log.Printf("DataChannel %s received: %s", d.Label(), string(msg.Data))

			var message map[string]interface{}
			if err := json.Unmarshal(msg.Data, &message); err == nil {
				if msgType, ok := message["type"].(string); ok {
					switch msgType {
					case "start_speak":
						s.mu.Lock()
						s.isSpeaking = true
						s.trackActive = true
						s.audioBuffer = make([][]byte, 0)
						s.mu.Unlock()

						s.stopTrackMu.Lock()
						s.stopTrack = make(chan struct{})
						s.stopTrackMu.Unlock()

						log.Printf("Started speaking - ready to receive audio")
						d.SendText("Started speaking")

					case "end_speak":
						s.mu.Lock()
						s.isSpeaking = false
						s.trackActive = false
						bufferCopy := make([][]byte, len(s.audioBuffer))
						copy(bufferCopy, s.audioBuffer)
						s.audioBuffer = make([][]byte, 0)
						s.mu.Unlock()

						s.stopTrackMu.Lock()
						close(s.stopTrack)
						s.stopTrackMu.Unlock()

						// Echo back all buffered audio
						go func(buffer [][]byte) {
							log.Printf("Starting to echo back %d audio packets", len(buffer))

							for _, packet := range buffer {
								rtpPacket := &rtp.Packet{}
								if err := rtpPacket.Unmarshal(packet); err != nil {
									log.Printf("Error unmarshaling RTP packet during playback: %v", err)
									continue
								}

								if err := s.audioTrack.WriteSample(media.Sample{
									Data:     rtpPacket.Payload,
									Duration: time.Millisecond * 20,
								}); err != nil {
									log.Printf("Error writing sample: %v", err)
								} else {
									log.Printf("Echoed audio packet: seq=%d, ts=%d, size=%d bytes",
										rtpPacket.SequenceNumber, rtpPacket.Timestamp, len(rtpPacket.Payload))
								}

								time.Sleep(time.Millisecond * 20)
							}
							log.Printf("Finished echoing back audio packets")
						}(bufferCopy)

						d.SendText("Ended speaking")
					}
				}
			}
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(answer)
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
