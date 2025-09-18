package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	transcribe "github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
	tstypes "github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"
)

/*
Learning note: Concurrency model in this file
=============================================

This file demonstrates Go's concurrency primitives to wrap the AWS Transcribe
Streaming SDK in a channel-first, goroutine-driven API.

Goroutines (go f())
-------------------
- Goroutines are lightweight concurrent functions managed by the Go runtime.
- You start one with the keyword `go` before a function call, e.g. `go worker()`.
- They run concurrently with the caller; the caller does not wait.

Channels (make(chan T, N))
--------------------------
- Channels are typed pipes for communication between goroutines.
- `make(chan T, N)` creates a channel that can buffer up to N values of type T
  (use N=0 for unbuffered synchronous handoff). When the buffer is full, sends
  block; when empty, receives block — this naturally creates backpressure.
- Close with `close(ch)` to signal no more values will be sent. Ranging over a
  channel finishes when the channel is closed and drained.

Channel direction arrows in types
---------------------------------
- `chan T`   : both send and receive permitted (bi-directional).
- `chan<- T` : send-only view (producer) — you can send but not receive.
- `<-chan T` : receive-only view (consumer) — you can receive but not send.
We use these to express intent in function signatures and return types so the
compiler helps catch misuse.

select over channels
--------------------
- `select { case x := <-ch1: ... case ch2 <- v: ... case <-ctx.Done(): ... }`
  waits on whichever case is ready first (receive, send, or cancellation).
- This is key for coordinating independent goroutines without busy waiting or
  fragile timing logic.

Non-blocking send with default
------------------------------
- `select { case errCh <- err: default: }` attempts to send once, and if the
  buffer is full (or no goroutine is receiving), it drops the value. We use
  this pattern for one-shot error channels where only the first error matters
  and we do not want to block and leak goroutines.
*/

const (
	// chunkMs controls pacing of audio chunks to simulate microphone cadence
	chunkMs = 100

	// Audio format constants for this demo
	sampleRateHz   = 16000 // AWS Transcribe expects 16kHz for US English
	bytesPerSample = 2     // 16-bit PCM
	numChannels    = 1     // mono (AWS Transcribe works better with mono)
)

type AudioChunk struct {
	PCM   []byte // raw PCM bytes (decoded)
	TsMs  int64  // simulated timestamp
	Final bool   // mark end-of-stream
}

type TranscriptPiece struct {
	Text    string
	Partial bool
}

// runTranscribeStream starts an AWS Transcribe Streaming session and wires it
// into three Go channels so callers can interact with the stream using
// idiomatic concurrency primitives instead of SDK calls.
//
// What this function does (high level):
// - Creates and returns:
//   - audioInputChannel (send-only to caller): you SEND AudioChunk values here.
//     This is how you push PCM audio into Transcribe. Sending a chunk with
//     Final=true indicates end-of-stream and causes the input side to close.
//   - transcriptOutputChannel (receive-only to caller): you RECEIVE
//     TranscriptPiece values here. Each piece represents either a partial or a
//     final transcript emitted by AWS.
//   - errOutputChannel (receive-only to caller): you RECEIVE errors here. Only
//     the first fatal error is delivered; subsequent errors are dropped to avoid
//     goroutine blocking.
//
// Buffer sizes and backpressure:
//   - audioInputChannel is buffered (16). If producers outpace the AWS sender,
//     writes will eventually block, applying natural backpressure.
//   - transcriptOutputChannel is buffered (32). If consumers outpace AWS receive,
//     reads will block until the consumer drains the channel.
//   - errOutputChannel is buffered (1). Only the first error is guaranteed to be
//     observed. This keeps error reporting simple and prevents deadlocks.
//
// Lifecycle and closing semantics:
//   - Caller owns audioInputChannel in the sense of sending values. Do NOT close
//     it directly. To signal completion, send an AudioChunk with Final=true. The
//     sender goroutine will close the underlying AWS stream.
//   - transcriptOutputChannel is closed by this function when the AWS receive side
//     completes (normal or error). Consumers can range over it to detect end.
//   - errOutputChannel is closed by this function when the session fully ends.
//
// Cancellation:
//   - Pass a context that will be canceled when the session should stop (e.g.,
//     when a WebSocket disconnects). Cancellation stops both send and receive
//     loops.

func runTranscribeStream(ctx context.Context, client *transcribe.Client) (chan<- AudioChunk, <-chan TranscriptPiece, <-chan error, error) {

	slog.Info("transcribe: starting session")
	stream, err := client.StartStreamTranscription(ctx, &transcribe.StartStreamTranscriptionInput{
		LanguageCode:         tstypes.LanguageCodeEnUs,
		MediaEncoding:        tstypes.MediaEncodingPcm,
		MediaSampleRateHertz: aws.Int32(sampleRateHz),
		// EnablePartialResultsStabilization: true,
		// PartialResultsStability:           tstypes.PartialResultsStabilityHigh,
	})
	if err != nil {
		slog.Error("transcribe: start failed", slog.String("error", err.Error()))
		return nil, nil, nil, err
	}

	slog.Info("transcribe: session started")

	// Channel where the caller will PRODUCE audio chunks for Transcribe.
	audioInputChannel := make(chan AudioChunk, 16)

	// Channel where the caller will CONSUME transcript pieces produced by AWS.
	transcriptOutputChannel := make(chan TranscriptPiece, 32)

	// Channel where the caller will CONSUME errors emitted by this session.
	errOutputChannel := make(chan error, 1)

	// Signal channels for internal coordination of completion.
	sendDone := make(chan error, 1)
	recvDone := make(chan error, 1)

	// Sender goroutine: reads AudioChunk from audioInputChannel and send to AWS Transcribe API.
	// Running this code won't block the execution, as it is running in a goroutine. There is no way of reading the return value of a function
	// running as a goroutine. That's what channels are for. The function communicates with other processes via channels.
	go func() {
		slog.Info("sender: started")
		defer close(sendDone)
		for ch := range audioInputChannel {
			// Final=true signals end-of-stream from the producer (e.g., client closed)
			if ch.Final {
				slog.Info("sender: received final", slog.Int64("ts_ms", ch.TsMs))
				_ = stream.GetStream().Close()
				return
			}

			// Forward PCM payload to AWS. We wrap the AudioEvent in the union type that
			// the SDK expects for the event stream.
			if err := stream.GetStream().Send(ctx, &tstypes.AudioStreamMemberAudioEvent{Value: tstypes.AudioEvent{AudioChunk: ch.PCM}}); err != nil {
				slog.Error("sender: send failed", slog.String("error", err.Error()))
				sendDone <- fmt.Errorf("send audio: %w", err)
				return
			}
			slog.Debug("sender: chunk sent", slog.Int("bytes", len(ch.PCM)), slog.Int64("ts_ms", ch.TsMs))
		}
		// If the producer closes audioInputChannel without sending Final, we still
		// close the AWS stream to release resources.
		slog.Info("sender: input channel closed; closing aws stream")
		_ = stream.GetStream().Close()
	}()

	// Receiver goroutine: reads transcript events from the AWS transcribe stream and sends them to the transcriptOutputChannel.
	// It is interesting to note that sender has no idea who the receiver is, and receiver has no idea who the sender is.
	go func() {
		slog.Info("receiver: started")
		defer close(recvDone)
		for ev := range stream.GetStream().Events() {
			switch te := ev.(type) {
			case *tstypes.TranscriptResultStreamMemberTranscriptEvent:
				if te.Value.Transcript == nil {
					slog.Debug("receiver: event without transcript")
					continue
				}
				for _, res := range te.Value.Transcript.Results {
					for _, alt := range res.Alternatives {
						if alt.Transcript != nil {
							slog.Debug("receiver: transcript piece", slog.Bool("partial", res.IsPartial))
							transcriptOutputChannel <- TranscriptPiece{Text: *alt.Transcript, Partial: res.IsPartial}
						}
					}
				}
			default:
				// ignore non-transcript events
				slog.Info("receiver: non-transcript event ignored", slog.String("type", fmt.Sprintf("%T", ev)))
			}
		}
		if err := stream.GetStream().Err(); err != nil {
			slog.Error("receiver: stream error", slog.String("error", err.Error()))
			recvDone <- fmt.Errorf("receive: %w", err)
			return
		}
		slog.Info("receiver: finished; no more events")
	}()

	// Closer goroutine: waits for either sender or receiver to finish, reports the
	// first error (if any), then closes transcriptOutputChannel and errOutputChannel
	// to signal completion to the caller.
	go func() {
		slog.Info("closer: waiting for completion")
		var firstErr error
		select {
		case e := <-sendDone:
			slog.Info("closer: sender finished", slog.Bool("error", e != nil))
			firstErr = e
		case e := <-recvDone:
			slog.Info("closer: receiver finished", slog.Bool("error", e != nil))
			firstErr = e
		}
		if firstErr != nil {
			select {
			case errOutputChannel <- firstErr:
			default:
			}
		}
		slog.Info("closer: closing output channels")
		close(transcriptOutputChannel)
		close(errOutputChannel)
	}()

	// Return the channels to the caller:
	// - audioInputChannel: caller sends AudioChunk values
	// - transcriptOutputChannel: caller receives TranscriptPiece values
	// - errOutputChannel: caller receives a terminal error (if any)
	slog.Info("transcribe: channels ready")
	return audioInputChannel, transcriptOutputChannel, errOutputChannel, nil
}
