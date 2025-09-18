package main

import (
	"fmt"
	"log/slog"
	"net/http"

	transcribe "github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
	"github.com/gorilla/websocket"
)

func ServeIndexPage() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	}
}

// StreamAudioEndpoint upgrades to WebSocket and bridges each connection to a new
// AWS Transcribe streaming session created via runTranscribeStream.
//
// Per-connection flow:
//   - Client sends binary audio frames (PCM 44.1kHz, stereo, 16-bit). We forward
//     them as AudioChunk values to the audioInput channel.
//   - We read TranscriptPiece values from transcriptOutput and write them back to
//     the WebSocket as text frames (you can wrap as JSON if preferred).
//   - A text frame with content "END" tells the server no more audio will come; we
//     send a Final=true chunk and close the session.
//   - Any error on the Transcribe session is logged and the connection is closed.
//
// Learning notes (applied here):
//   - We create a per-connection goroutine to READ from the socket and SEND into
//     the audioInput channel. This isolates IO from compute and avoids blocking the
//     writer.
//   - We use a `select` loop to WAIT on transcript output, errors, or cancellation.
//     This lets the server react to whichever event happens first without busy wait.
//   - The channel direction arrows in the signature of runTranscribeStream enforce
//     usage: we can only send AudioChunk values into audioIn, and only receive
//     TranscriptPiece values from transcriptOut.
//
// Why reading from WebSocket needs a goroutine:
//  1. Concurrent I/O: WebSocket connections need to handle reading and writing
//     simultaneously. Without a separate goroutine for reading, we would block the
//     main thread while waiting for incoming messages, preventing us from sending
//     transcripts back to the client.
//  2. Avoiding Deadlocks: The WebSocket protocol requires handling of control frames
//     (like ping/pong) even while processing application data. A dedicated read
//     goroutine ensures we can always respond to these control frames.
//  3. Backpressure Management: Using a goroutine with channels creates natural
//     backpressure - if the audioIn channel gets full, the reader will block
//     until there's space, without blocking the transcript writing path.
func StreamAudioEndpoint(client *transcribe.Client) http.HandlerFunc {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	return func(w http.ResponseWriter, r *http.Request) {
		slog.Info("ws: connection upgrading", slog.String("remote", r.RemoteAddr))
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.Error("Error upgrading to WebSocket:", slog.String("error", err.Error()))
			return
		}
		defer conn.Close()
		slog.Info("ws: connection established", slog.String("remote", r.RemoteAddr))

		// Use the request context for cancellation when the client disconnects.
		ctx := r.Context()

		// Start a per-connection Transcribe session and obtain channels.
		audioIn, transcriptOut, errOut, err := runTranscribeStream(ctx, client)
		if err != nil {
			slog.Error("ws: transcribe stream error", slog.String("error", err.Error()))
			return
		}

		go func() {
			slog.Info("ws-reader: started", slog.String("remote", r.RemoteAddr))
			var tsMs int64 = 0
			for {
				mt, data, err := conn.ReadMessage()
				if err != nil {
					slog.Warn("ws-reader: read error; signaling final", slog.String("error", err.Error()))
					audioIn <- AudioChunk{Final: true, TsMs: tsMs}
					return
				}
				switch mt {

				// If the client sends binary data (the audio chunks we are looking for),
				// we copy it to a new slice and send it to the audioInput channel.
				case websocket.BinaryMessage:
					// We must copy the binary data to a new slice because WebSocket's ReadMessage()
					// reuses its internal buffer. If we sent 'data' directly to the channel,
					// the next ReadMessage() call would overwrite the bytes before they're processed.
					// By copying to a new slice, we ensure each AudioChunk owns its PCM data.
					payload := make([]byte, len(data))
					copy(payload, data)
					audioIn <- AudioChunk{PCM: payload, TsMs: tsMs}
					tsMs += chunkMs

				// If the client sends "END", we signal the end of the stream with a Final=true AudioChunk.
				// We break the loop and return, finishing the goroutine.
				case websocket.TextMessage:
					if string(data) == "END" {
						audioIn <- AudioChunk{Final: true, TsMs: tsMs}
						slog.Info("ws-reader: received END; signaling final and stopping")
						return
					}
				default:
					// Ignore WebSocket control frames like ping/pong
					// These are handled automatically by the WebSocket library
				}
			}
		}()

		// Writer loop: transcriptOut/errOut -> WS
		slog.Info("ws-writer: started", slog.String("remote", r.RemoteAddr))
		for {
			select {
			case piece, ok := <-transcriptOut:
				if !ok {
					slog.Info("ws-writer: transcript channel closed; stopping")
					return
				}
				// Send transcript as JSON with partial flag
				jsonMsg := fmt.Sprintf(`{"text":"%s","partial":%t}`, piece.Text, piece.Partial)
				if err := conn.WriteMessage(websocket.TextMessage, []byte(jsonMsg)); err != nil {
					slog.Error("ws-writer: write failed", slog.String("error", err.Error()))
					return
				}
				slog.Info("ws-writer: transcript sent", slog.Bool("partial", piece.Partial), slog.String("text", piece.Text))
			case err, ok := <-errOut:
				if ok && err != nil {
					slog.Error("ws-writer: transcribe error", slog.String("error", err.Error()))
				}
				return
			case <-ctx.Done():
				slog.Info("ws-writer: context done; closing connection")
				return
			}
		}
	}
}
