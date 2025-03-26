# WebRTC Demo

A real-time audio streaming demo using WebRTC technology. This project demonstrates how to establish peer-to-peer connections and stream audio between a Go server and a web client.

## Features

- Real-time audio streaming using WebRTC
- WebSocket-based signaling server
- Web client interface for audio interaction
- Support for audio track management and streaming

## Prerequisites

- Go 1.16 or higher
- Modern web browser with WebRTC support
- Git

## Installation

1. Clone the repository:
```bash
git clone https://github.com/yourusername/webrtc-demo.git
cd webrtc-demo
```

2. Install dependencies:
```bash
go mod download
```

## Running the Application

1. Start the server:
```bash
go run main.go
```

2. Open your web browser and navigate to:
```
http://localhost:80
```

3. Click the "Start Session" button to start a webrtc connection

4. Click the "Start Speak" button to start recording.

5. Click the "End Speak" button and it is expected to hear the recording echoing back.

## Project Structure

- `main.go`: Contains the WebRTC server implementation and WebSocket handling
- `client.html`: Web client interface for audio interaction
- `go.mod` & `go.sum`: Go module dependency management files

## Dependencies

- [Pion WebRTC](https://github.com/pion/webrtc): WebRTC implementation for Go
- [Gorilla WebSocket](https://github.com/gorilla/websocket): WebSocket implementation for Go

## How It Works

1. The server establishes a WebSocket connection with the client
2. WebRTC peer connection is created between server and client
3. Audio tracks are established and managed
4. Real-time audio streaming begins between peers

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License - see the LICENSE file for details.

## Acknowledgments

- [Pion WebRTC](https://github.com/pion/webrtc) for the excellent WebRTC implementation
- [Gorilla WebSocket](https://github.com/gorilla/websocket) for the WebSocket implementation 