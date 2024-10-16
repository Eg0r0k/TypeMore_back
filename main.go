package main

import (
	"log"
	"net/http"
	"typeMore/api"
	"typeMore/config"
	"typeMore/db"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)


func init() {
    err := godotenv.Load()
    if err != nil {
        log.Fatal("Error loading .env file")
    }
}

func main() {
        db, err := db.Connect(config.Load())
        if err != nil {
                log.Fatalf("Failed to connect to the database: %v", err)
        }
        defer db.Close()   
        router := api.SetupRoutes(db)

        log.Println("Starting server on port", config.Load().ServerPort)
        log.Fatal(http.ListenAndServe(":"+config.Load().ServerPort, router))
}