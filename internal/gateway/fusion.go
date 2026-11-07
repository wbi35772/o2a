package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
)

// fusedPool names one of the two anonymous egress pools.
type fusedPool int

const (
	fusedWARP fusedPool = iota
	fusedStatic
)

// fusedTie alternates equally sized pools so neither starves the other.
var fusedTie atomic.Uint64

// fusedActive reports whether this request can draw from both pools: the WARP
// pool is enabled for the model and the anonymous list holds at least one exit
// that is not the host's own direct route.
func (g *Gateway) fusedActive(route models.Route) bool {
	if g == nil || g.warp == nil || !g.usesWARP(route.ID, config.TierZen, true) {
		return false
	}
	return g.anonymous.HasExternal()
}

// warpAvailable estimates WARP exits that can serve right now. The pool has no
// Available accessor; Active minus Cooling is the same number Snapshot shows.
func (g *Gateway) warpAvailable() int {
	if g.warp == nil {
		return 0
	}
	s := g.warp.Snapshot()
	if s.Active <= s.Cooling {
		return 0
	}
	return s.Active - s.Cooling
}

// fusedOrder picks which pool this request tries first. A pool with no usable
// exit yields to the other; a pool with at least twice the warm capacity is
// preferred; otherwise the pools alternate through an atomic counter. A static
// pool whose exits are all in a hard 429 hold reports zero and yields to WARP.
func (g *Gateway) fusedOrder(now time.Time) (fusedPool, fusedPool) {
	warpN := g.warpAvailable()
	staticN := g.anonymous.AvailableExits(now)
	switch {
	case warpN == 0 && staticN == 0:
		return fusedWARP, fusedStatic
	case warpN == 0:
		return fusedStatic, fusedWARP
	case staticN == 0:
		return fusedWARP, fusedStatic
	case warpN >= 2*staticN:
		return fusedWARP, fusedStatic
	case staticN >= 2*warpN:
		return fusedStatic, fusedWARP
	case fusedTie.Add(1)%2 == 1:
		return fusedWARP, fusedStatic
	default:
		return fusedStatic, fusedWARP
	}
}

// doFusedAnonymousUpstream runs the preferred pool, then the other one when the
// first cannot produce a 2xx. Both walks keep their own health accounting; the
// caller only sees one response and one error, with the same error precedence
// the single-pool lanes already use. The response the first pool could not
// serve is drained and closed before the second pool starts, so a fallback
// never leaks an upstream connection.
func (g *Gateway) doFusedAnonymousUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, error, int) {
	first, second := g.fusedOrder(time.Now())
	resp, err, attempts := g.fusedWalk(first, ctx, route, bodies, ids, attemptOffset)
	if resp != nil && resp.StatusCode/100 == 2 {
		return resp, nil, attempts
	}
	if errors.Is(err, errDeterministicClientRejection) || ctx.Err() != nil {
		return resp, err, attempts
	}
	if resp != nil {
		httpx.DrainAndClose(resp.Body)
	}
	g.logger.Info("anonymous pools fused; trying the other pool",
		"component", "upstream", "event", "fused_pool_fallback",
		"request_id", ids.Request, "from", fusedPoolName(first),
		"to", fusedPoolName(second), "attempts", attempts,
		"error", err)
	resp2, err2, used2 := g.fusedWalk(second, ctx, route, bodies, ids, attemptOffset+attempts)
	attempts += used2
	if resp2 != nil && resp2.StatusCode/100 == 2 {
		return resp2, nil, attempts
	}
	if errors.Is(err2, errDeterministicClientRejection) {
		return resp2, err2, attempts
	}
	// A spent free-tier quota on either pool must stay visible: doUpstreamTiers
	// uses it to skip the Zen key tier, which would answer 429 again.
	if errors.Is(err, errFreeTierIPBlocked) || errors.Is(err2, errFreeTierIPBlocked) {
		return resp2, errFreeTierIPBlocked, attempts
	}
	if resp2 != nil {
		return resp2, err2, attempts
	}
	if err2 != nil {
		return nil, err2, attempts
	}
	return nil, err, attempts
}

func (g *Gateway) fusedWalk(pool fusedPool, ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, offset int) (*http.Response, error, int) {
	if pool == fusedWARP {
		// allowDirect=false: the host's own IPv4 must stay behind the external
		// pool, not in front of it.
		return g.warpAnonymousWalk(ctx, route, bodies, ids, offset, false)
	}
	return g.doStaticAnonymousUpstream(ctx, route, bodies, ids, offset)
}

func fusedPoolName(pool fusedPool) string {
	if pool == fusedWARP {
		return "warp"
	}
	return "static"
}
