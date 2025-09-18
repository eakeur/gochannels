package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/config"

	transcribe "github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile("CaylentDev"), config.WithRegion("us-east-1"))
	if err != nil {
		slog.Error("aws cfg load failed", slog.String("error", err.Error()))
		log.Fatalf("aws cfg: %v", err)
	}

	client := transcribe.NewFromConfig(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", StreamAudioEndpoint(client))
	mux.HandleFunc("/", ServeIndexPage())
	mux.HandleFunc("/audio.mp3", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "darling-hold-my-hand.mp3")
	})

	server := &http.Server{Addr: ":8080", Handler: mux}

	go func() {
		slog.Info("http: server start", slog.String("addr", server.Addr))
		<-ctx.Done()
		_ = server.Close()
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http: server error", slog.String("error", err.Error()))
		panic(err)
	}
	slog.Info("http: server stopped")
}
