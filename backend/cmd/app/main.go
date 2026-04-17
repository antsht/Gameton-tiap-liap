package main

import (
	"log"
	"os"
	"strings"

	"piratesbot/internal/api"
	"piratesbot/internal/bot"
	"piratesbot/internal/server"
)

func main() {
	// keyPath := `d:\!PROGRAMMING\!pirates\TASK\X-API-Key`
	// keyBytes, err := os.ReadFile(keyPath)
	// if err != nil {
	// 	log.Fatalf("Could not read API Key from %s: %v", keyPath, err)
	// }
	token := "34c69968-5334-4619-8253-57433ac44e7d"

	log.Printf("Read API Token, len %d", len(token))

	// According to docs, test server is https://games-test.datsteam.dev
	client := api.NewClient("https://games-test.datsteam.dev", token)
	b := bot.NewBot(client)

	srv := server.NewServer(":8080", b)

	log.Println("Starting Web server on http://localhost:8080")
	if err := srv.Start(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
