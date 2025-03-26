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

	"github.com/pion/webrtc/v4"
)

type WebRTCServer struct {
	peerConnection *webrtc.PeerConnection
	mu             sync.Mutex
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
					"stun:stun2.l.google.com:19302",
				},
			},
		},
	}

	return webrtc.NewPeerConnection(config)
}

func (s *WebRTCServer) handleOffer(w http.ResponseWriter, r *http.Request) {
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		log.Printf("Error decoding offer: %v", err)
		http.Error(w, "Failed to decode offer", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	var err error
	s.peerConnection, err = s.createPeerConnection()
	s.mu.Unlock()

	if err != nil {
		log.Printf("Error creating peer connection: %v", err)
		http.Error(w, "Failed to create peer connection", http.StatusInternalServerError)
		return
	}

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
