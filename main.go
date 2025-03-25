package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/rtp"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Allow all origins for demo
		},
	}
)

type WebRTCServer struct {
	peerConnection *webrtc.PeerConnection
	dataChannel   *webrtc.DataChannel
	audioTrack    *webrtc.TrackLocalStaticSample
	mu            sync.Mutex
	isSpeaking    bool
	audioBuffer   [][]byte
	stopTrack     chan struct{}
	trackActive   bool
	stopTrackMu   sync.Mutex  // New mutex to protect stopTrack channel
}

func NewWebRTCServer() *WebRTCServer {
	return &WebRTCServer{
		audioBuffer: make([][]byte, 0),
		stopTrack:   make(chan struct{}),
		trackActive: false,
	}
}

func (s *WebRTCServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade connection: %v", err)
		return
	}
	defer conn.Close()

	// Create a new PeerConnection
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log.Printf("Failed to create peer connection: %v", err)
		return
	}
	defer peerConnection.Close()

	// Create audio track for echo
	audioTrack, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion")
	if err != nil {
		log.Printf("Failed to create audio track: %v", err)
		return
	}

	s.mu.Lock()
	s.peerConnection = peerConnection
	s.audioTrack = audioTrack
	s.mu.Unlock()

	// Add the audio track to the peer connection
	_, err = peerConnection.AddTrack(audioTrack)
	if err != nil {
		log.Printf("Failed to add audio track: %v", err)
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
				// Don't wait for next signal, just continue the loop
				continue
			default:
				// Continue processing
			}
		}
	})

	// Handle incoming data channel
	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
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

	// Handle ICE candidates
	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		candidateJSON, err := json.Marshal(c.ToJSON())
		if err != nil {
			log.Printf("Failed to marshal ICE candidate: %v", err)
			return
		}

		err = conn.WriteMessage(websocket.TextMessage, candidateJSON)
		if err != nil {
			log.Printf("Failed to send ICE candidate: %v", err)
		}
	})

	// Handle incoming messages
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Failed to read message: %v", err)
			return
		}

		if messageType == websocket.TextMessage {
			var msg map[string]interface{}
			if err := json.Unmarshal(message, &msg); err != nil {
				log.Printf("Failed to unmarshal message: %v", err)
				continue
			}

			switch msg["type"].(string) {
			case "offer":
				offer := webrtc.SessionDescription{
					Type: webrtc.SDPTypeOffer,
					SDP:  msg["sdp"].(map[string]interface{})["sdp"].(string),
				}

				err = peerConnection.SetRemoteDescription(offer)
				if err != nil {
					log.Printf("Failed to set remote description: %v", err)
					continue
				}

				answer, err := peerConnection.CreateAnswer(nil)
				if err != nil {
					log.Printf("Failed to create answer: %v", err)
					continue
				}

				err = peerConnection.SetLocalDescription(answer)
				if err != nil {
					log.Printf("Failed to set local description: %v", err)
					continue
				}

				answerJSON, err := json.Marshal(answer)
				if err != nil {
					log.Printf("Failed to marshal answer: %v", err)
					continue
				}

				err = conn.WriteMessage(websocket.TextMessage, answerJSON)
				if err != nil {
					log.Printf("Failed to send answer: %v", err)
				}

			case "ice-candidate":
				candidate := webrtc.ICECandidateInit{}
				candidateJSON, err := json.Marshal(msg["candidate"])
				if err != nil {
					log.Printf("Failed to marshal ICE candidate: %v", err)
					continue
				}

				err = json.Unmarshal(candidateJSON, &candidate)
				if err != nil {
					log.Printf("Failed to unmarshal ICE candidate: %v", err)
					continue
				}

				err = peerConnection.AddICECandidate(candidate)
				if err != nil {
					log.Printf("Failed to add ICE candidate: %v", err)
				}
			}
		}
	}
}

func main() {
	port := flag.String("port", "8080", "Port to listen on")
	flag.Parse()

	server := NewWebRTCServer()

	// Serve static files
	fs := http.FileServer(http.Dir("."))
	http.Handle("/", fs)

	// WebSocket endpoint
	http.HandleFunc("/ws", server.handleWebSocket)

	log.Printf("Server starting on port %s...", *port)
	if err := http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatal(err)
	}
} 