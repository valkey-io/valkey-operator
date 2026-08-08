package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/valkey-io/valkey-go"
)

func main() {
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		log.Fatal("VALKEY_ADDR not set")
	}
	numKeys, _ := strconv.Atoi(os.Getenv("NUM_KEYS"))
	if numKeys <= 0 {
		numKeys = 100000
	}
	dataSize, _ := strconv.Atoi(os.Getenv("DATA_SIZE"))
	if dataSize <= 0 {
		dataSize = 3
	}
	rps, _ := strconv.Atoi(os.Getenv("RPS"))
	if rps <= 0 {
		rps = 20
	}
	value := strings.Repeat("x", dataSize)

	log.Printf("Connecting to %s...\n", addr)
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{addr},
	})
	if err != nil {
		log.Fatalf("connect failed: %v\n", err)
	}
	defer client.Close()

	ctx := context.Background()

	// Phase 1: Seed all keys
	log.Printf("SEEDING %d keys...\n", numKeys)
	seeded := 0
	for i := range numKeys {
		key := fmt.Sprintf("key:%012d", i)
		err := client.Do(ctx, client.B().Set().Key(key).Value(value).Build()).Error()
		if err != nil {
			log.Printf("seed error at key %d: %v\n", i, err)
			continue
		}
		seeded++
	}
	log.Printf("SEEDED %d\n", seeded)

	// Phase 2: Continuous updates at target RPS (or exit if RPS=0)
	if rps <= 0 {
		log.Println("RPS=0, seed-only mode, exiting")
		return
	}

	var writes, errors atomic.Int64
	interval := time.Second / time.Duration(rps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Print stats every 5 seconds
	go func() {
		for range time.Tick(5 * time.Second) {
			w := writes.Load()
			e := errors.Load()
			log.Printf("writes=%d errors=%d rps=%.1f\n", w, e, float64(rps))
		}
	}()

	keyIdx := 0
	for range ticker.C {
		key := fmt.Sprintf("key:%012d", keyIdx%numKeys)
		keyIdx++

		err := client.Do(ctx, client.B().Set().Key(key).Value(value).Build()).Error()
		if err != nil {
			errors.Add(1)
			continue
		}
		writes.Add(1)
	}
}
