package gateway

import (
	"strconv"
	"sync"

	"opencode2api/internal/identity"
)

// sessionGuard tracks the upstream free-tier session identity of each
// conversation and moves it to a fresh generation when the upstream reports
// the session's quota as spent.
//
// The free tier ties its quota and risk bookkeeping to the session id, not only
// to the egress address: a conversation that has exhausted its free allowance
// keeps answering HTTP 429 "Free usage limit reached" on every fresh WARP exit,
// while a new session on the same exit succeeds. Rotating the exit in that
// situation burns the whole pool for 24 hours without fixing the request.
//
// Generation zero is the id derived from the client conversation. Every burn
// increments the generation; the id sent upstream is a stable, canonical
// ses_... value derived from (base, generation), so every later request of the
// same conversation uses the same replacement session and keeps prompt-cache
// affinity until that one is spent too.
type sessionGuard struct {
	mu   sync.Mutex
	gens map[string]int
}

// sessionGuardMaxEntries bounds the per-process generation table. Conversations
// are cheap to re-derive after a restart; the cap only stops an unbounded map
// in a very long-lived process.
const sessionGuardMaxEntries = 4096

func newSessionGuard() *sessionGuard {
	return &sessionGuard{gens: make(map[string]int)}
}

// Resolve returns the session id currently assigned to base: base itself for
// generation zero, or the derived replacement after one or more burns.
func (s *sessionGuard) Resolve(base string) string {
	if s == nil || base == "" {
		return base
	}
	s.mu.Lock()
	gen := s.gens[base]
	s.mu.Unlock()
	return rotatedSessionID(base, gen)
}

// Rotate burns base's current generation and returns the next one. Concurrent
// requests for the same conversation each advance the counter; every caller
// resolves the latest generation, so they converge on one replacement id.
func (s *sessionGuard) Rotate(base string) string {
	if s == nil || base == "" {
		return base
	}
	s.mu.Lock()
	if len(s.gens) >= sessionGuardMaxEntries {
		// Drop an arbitrary entry rather than grow without bound. The affected
		// conversation falls back to generation zero, which is no worse than a
		// restart and only costs one more 429 if that session is still spent.
		for key := range s.gens {
			delete(s.gens, key)
			break
		}
	}
	s.gens[base]++
	gen := s.gens[base]
	s.mu.Unlock()
	return rotatedSessionID(base, gen)
}

// Generation reports how many times base has been rotated in this process.
func (s *sessionGuard) Generation(base string) int {
	if s == nil || base == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gens[base]
}

// ResolveIDs rewrites the session headers of ids to their current generation.
// A child session is resolved on its own key, except when it is the same value
// as the parent: then it must keep following the parent's rotation.
func (s *sessionGuard) ResolveIDs(ids identity.RequestIDs) identity.RequestIDs {
	baseSession, baseChild := ids.Session, ids.ChildSession
	ids.Session = s.Resolve(baseSession)
	if baseChild != "" {
		if baseChild == baseSession {
			ids.ChildSession = ids.Session
		} else {
			ids.ChildSession = s.Resolve(baseChild)
		}
	}
	return ids
}

// RotateIDs burns the current generation of every identity in ids and returns
// the replacement ids. The base ids must be the original, unrotated values.
func (s *sessionGuard) RotateIDs(ids identity.RequestIDs) identity.RequestIDs {
	baseSession, baseChild := ids.Session, ids.ChildSession
	ids.Session = s.Rotate(baseSession)
	if baseChild != "" {
		if baseChild == baseSession {
			ids.ChildSession = ids.Session
		} else {
			ids.ChildSession = s.Rotate(baseChild)
		}
	}
	return ids
}

// rotatedSessionID derives the canonical upstream id for one generation of a
// conversation. Generation zero is the conversation id itself.
func rotatedSessionID(base string, generation int) string {
	if generation <= 0 || base == "" {
		return base
	}
	return identity.CanonicalSessionID(base + "\x00session-rotation\x00" + strconv.Itoa(generation))
}
