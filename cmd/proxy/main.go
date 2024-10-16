package main

import (
	"evm-cache/internal/api"
	"evm-cache/internal/config"
	"evm-cache/internal/logging"
	"flag"
	"log"
)

func main() {
	configFile := flag.String("config", "", "Path to the YAML configuration file")
	flag.Parse()

	cfg, err := config.Load(*configFile)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	if err := logging.Setup(cfg); err != nil {
		log.Fatalf("Failed to setup logging: %v", err)
	}

	logger := logging.GetLogger()

	server, err := api.NewServer(cfg)
	if err != nil {
		logger.Fatalf("Failed to initialize server: %v", err)
	}

	server.Start()
}
