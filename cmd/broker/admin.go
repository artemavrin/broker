package main

import (
	"context"
	"fmt"
	"time"

	"github.com/artemavrin/broker/internal/config"
	"github.com/artemavrin/broker/internal/db"
	"github.com/artemavrin/broker/internal/secret"
)

// runCreateInitiator provisions a fresh initiator, storing only the hash of a
// newly generated secret and printing the id and raw secret to stdout exactly
// once. The secret cannot be recovered later.
func runCreateInitiator() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	database, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()

	if err := database.Migrate(ctx, cfg.MigrationsDir); err != nil {
		return err
	}

	raw, err := secret.Generate()
	if err != nil {
		return err
	}
	id, err := database.CreateInitiator(ctx, secret.Hash(raw))
	if err != nil {
		return err
	}

	fmt.Println("initiator created — store the secret now, it will not be shown again:")
	fmt.Printf("  initiator_id:     %s\n", id)
	fmt.Printf("  initiator_secret: %s\n", raw)
	return nil
}
