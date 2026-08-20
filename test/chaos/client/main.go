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

// envIntOrDefault returns the integer value of the env var, or defaultVal when
// unset. An explicit zero is preserved; an unparsable or too small value is fatal.
func envIntOrDefault(key string, defaultVal, minVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("invalid %s %q: %v", key, v, err)
	}
	if n < minVal {
		log.Fatalf("%s=%d must be >= %d", key, n, minVal)
	}
	return n
}

func main() {
	addr := os.Getenv("VALKEY_ADDR")
	if addr == "" {
		log.Fatal("VALKEY_ADDR not set")
	}
	numKeys := envIntOrDefault("NUM_KEYS", 100000, 1 /* min */)
	dataSize := envIntOrDefault("DATA_SIZE", 3, 1 /* min */)
	rps := envIntOrDefault("RPS", 20, 0 /* min, 0 = seed only */)
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

	// Phase 1: Seed all keys. The cluster is Ready and reports healthy before the
	// client starts, so a refused write means the operator signalled readiness too
	// early. Fail rather than retry, so the suite stops and the cause is visible.
	log.Printf("SEEDING %d keys...\n", numKeys)
	seeded := 0
	for i := range numKeys {
		key := fmt.Sprintf("key:%012d", i)
		if err := client.Do(ctx, client.B().Set().Key(key).Value(value).Build()).Error(); err != nil {
			log.Fatalf("SEED FAILED at key %d of %d: %v\n", i, numKeys, err)
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
