package main

import (
	"math/rand"
	"sync"

	"github.com/google/uuid"
)

// ThreadSafeUUID is a thread-safe random number generator
type ThreadSafeUUID struct {
	rand *rand.Rand
	mu   sync.Mutex
}

// NewThreadSafeUUID creates a new thread-safe random number generator with a seed
func NewThreadSafeUUID(seed int64) *ThreadSafeUUID {
	return &ThreadSafeUUID{
		rand: rand.New(rand.NewSource(seed)), // Seed the random generator
	}
}

func (tsr *ThreadSafeUUID) generateUUIDv4() uuid.UUID {
	var buf [16]byte

	tsr.mu.Lock()
	// Read random bytes into the buffer using the seeded random source
	for i := 0; i < 16; i++ {
		buf[i] = byte(tsr.rand.Intn(256))
	}
	tsr.mu.Unlock()

	// Set the version (4) and variant bits
	buf[6] = (buf[6] & 0x0f) | 0x40 // Version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // Variant is 10

	// Convert the buffer to a UUID
	return uuid.UUID(buf)
}
