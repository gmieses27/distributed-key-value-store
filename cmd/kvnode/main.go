package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/gmieses27/distributed-kvstore/internal/server"
)

func main() {
	id := flag.Int("id", 1, "Node ID (1-based)")
	addr := flag.String("addr", ":8001", "Address to listen on, e.g. :8001")
	peersFlag := flag.String("peers", "", "Comma-separated peer addresses, e.g. localhost:8002,localhost:8003")
	dataDir := flag.String("data", "data", "Directory to store persistent state")
	flag.Parse()

	if *id < 1 {
		fmt.Fprintln(os.Stderr, "node id must be >= 1")
		os.Exit(1)
	}

	peers := []string{}
	if *peersFlag != "" {
		for _, p := range strings.Split(*peersFlag, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				peers = append(peers, p)
			}
		}
	}

	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		log.Fatalf("cannot create data dir: %v", err)
	}
	persistPath := filepath.Join(*dataDir, fmt.Sprintf("node%d.json", *id))

	srv := server.New(*id, *addr, peers, persistPath)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
