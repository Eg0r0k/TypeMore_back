package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	"typeMore/api"
	"typeMore/config"
	"typeMore/db"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"go.uber.org/zap"
)


func init() {
    err := godotenv.Load()
    if err != nil {
        log.Fatal("Error loading .env file")
    }
}

func main() {
    logger, _ := zap.NewProduction()
    defer logger.Sync()
    sugar := logger.Sugar()
    err := godotenv.Load()
    if err != nil {
            sugar.Fatalw("Error loading .env file", "error", err)
    }
    cfg := config.Load()
    db, err := db.Connect(cfg)
    if err != nil {
        sugar.Fatalw("Failed to connect to the database", "error", err)
}
    defer db.Close()   
    router := api.SetupRoutes(db, logger)
    server := &http.Server{
        Addr:         ":" + cfg.ServerPort,
        Handler:      router,
        ReadTimeout:  5 * time.Second,
        WriteTimeout: 0,
        IdleTimeout:  120 * time.Second,
    }
    go func() {
        sugar.Infow("Starting server", "port", cfg.ServerPort)
        if err := server.ListenAndServe(); err != http.ErrServerClosed {
                sugar.Fatalw("Server error", "error", err)
        }
}()
quit := make(chan os.Signal, 1)
signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
<-quit

sugar.Info("Shutting down server...")
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := server.Shutdown(ctx); err != nil {
        sugar.Fatalw("Server forced to shutdown", "error", err)
}

sugar.Info("Server stopped")
}