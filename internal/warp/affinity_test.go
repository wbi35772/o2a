package warp

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

// testExit builds the smallest exit addRef/retire need: an identity and an
// egress key so affinity bookkeeping can be exercised without a tunnel.
func testExit(id string) *exit {
	return &exit{id: id, egress: netip.MustParseAddr("2606:4700:4700::1"), stack: StackIPv6}
}

// affinityPool builds the smallest pool AcquireFor needs: a fixed ring, no
// registrar, and no refill trigger (size keeps desired at the ring length).
func affinityPool(exits ...*exit) *Pool {
	return &Pool{
		ctx:      context.Background(),
		size:     len(exits) + 8,
		active:   exits,
		affinity: make(map[string]*exit),
		reserved: make(map[string]bool),
		cooling:  make(map[string]time.Time),
		changed:  make(chan struct{}),
		newExit: func(context.Context) (*exit, error) {
			return nil, ErrNoExit
		},
	}
}

func TestAcquireForPinsConversation(t *testing.T) {
	a, b := testExit("a"), testExit("b")
	pool := affinityPool(a, b)

	first, err := pool.AcquireFor(context.Background(), "conv")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	pinned := first.exit
	first.Release()

	for turn := 0; turn < 5; turn++ {
		lease, err := pool.AcquireFor(context.Background(), "conv")
		if err != nil {
			t.Fatalf("turn %d acquire: %v", turn, err)
		}
		if lease.exit != pinned {
			t.Fatalf("turn %d moved to a different exit", turn)
		}
		lease.Release()
	}

	other, err := pool.AcquireFor(context.Background(), "another conversation")
	if err != nil {
		t.Fatalf("other acquire: %v", err)
	}
	if other.exit == pinned {
		t.Fatal("a second conversation reused the pinned exit")
	}
	other.Release()
}

func TestAcquireForRebindsRetiredExit(t *testing.T) {
	a, b := testExit("a"), testExit("b")
	pool := affinityPool(a, b)

	lease, err := pool.AcquireFor(context.Background(), "conv")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	pinned := lease.exit
	lease.Release()

	// A retirement (429 penalty, scale-down, failed dial) is what removes an
	// exit from service; addRef then refuses it and the pin must follow.
	pinned.retired = true

	next, err := pool.AcquireFor(context.Background(), "conv")
	if err != nil {
		t.Fatalf("acquire after retirement: %v", err)
	}
	defer next.Release()
	if next.exit == pinned {
		t.Fatal("conversation stayed on the retired exit")
	}
	if got := pool.affinity["conv"]; got != next.exit {
		t.Fatal("affinity was not rebound to the replacement exit")
	}
}

func TestAcquireWithoutAffinityRoundRobins(t *testing.T) {
	a, b := testExit("a"), testExit("b")
	pool := affinityPool(a, b)

	first, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	first.Release()
	second, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	second.Release()

	if first.exit == second.exit {
		t.Fatal("unkeyed acquires did not rotate the ring")
	}
	if len(pool.affinity) != 0 {
		t.Fatalf("unkeyed acquires wrote affinity: %d", len(pool.affinity))
	}
}
