package mitm

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestRejectedBidiAppendsDoNotReserveRouteSlots(t *testing.T) {
	routes := newBidiRouteRegistry()
	defer routes.close()
	req := routeAppendRequest("existing", 0, []byte{10, 1, 'a'}, false)
	_, resp := routes.append(req)
	closeRouteResponse(resp)
	if resp == nil || resp.StatusCode != 200 {
		t.Fatal("initial append failed")
	}
	existing := routes.entries[bidiRouteKey(req, "existing")]
	// Model a queue already at its byte budget without allocating 64 MiB.
	routes.queuedBytes = bidiRouteLimit
	for i := 0; i < 260; i++ {
		_, resp := routes.append(routeAppendRequest(fmt.Sprintf("rejected-%d", i), 0, []byte{10, 1, 'a'}, false))
		closeRouteResponse(resp)
		if resp == nil || resp.StatusCode != 429 {
			t.Fatalf("rejection %d: %v", i, resp)
		}
	}
	_, resp = routes.append(routeAppendRequest("existing", 1, []byte{10, 1, 'b'}, false))
	closeRouteResponse(resp)
	if resp == nil || resp.StatusCode != 429 || routes.entries[bidiRouteKey(req, "existing")] != existing || len(routes.entries) != 1 {
		t.Fatal("rejection changed existing routes or reserved new ones")
	}
	routes.queuedBytes = 0
	_, resp = routes.append(routeAppendRequest("after-drain", 0, []byte{10, 1, 'a'}, false))
	closeRouteResponse(resp)
	if resp == nil || resp.StatusCode != 200 {
		t.Fatal("released byte budget did not admit a new route")
	}
}

func TestUnclaimedBidiSlotsCannotBlockStreamClaims(t *testing.T) {
	routes := newBidiRouteRegistry()
	defer routes.close()
	claim := func(id string) {
		t.Helper()
		_, blocked, lease := routes.claim(routeStreamRequest(id, inferenceSSEPath, false), "local", false, context.Background(), nil)
		if blocked != nil || lease == nil {
			t.Fatalf("claim %s was blocked: %v", id, blocked)
		}
		t.Cleanup(lease.close)
	}
	for i := 0; i < 256-bidiStagingEntries; i++ {
		claim(fmt.Sprintf("established-%d", i))
	}
	for i := 0; i < bidiStagingEntries; i++ {
		req := routeAppendRequest(fmt.Sprintf("unverified-%d", i), 0, []byte{10, 1, 'a'}, false)
		req.Header.Set("Authorization", "Bearer unverified-account")
		_, response := routes.append(req, func(context.Context, http.Header) bool { t.Fatal("staging read local account"); return false })
		closeRouteResponse(response)
		if response == nil || response.StatusCode != 200 {
			t.Fatalf("staging %d failed", i)
		}
	}
	_, response := routes.append(routeAppendRequest("over-staging-limit", 0, []byte{10, 1, 'a'}, false))
	closeRouteResponse(response)
	if response == nil || response.StatusCode != 429 {
		t.Fatal("unclaimed slot limit was not enforced")
	}
	before := routes.queuedBytes
	claim("legitimate-new-stream")
	if len(routes.entries) != 256 || routes.queuedBytes >= before {
		t.Fatal("claim did not reclaim staging capacity")
	}
	for i := 0; i < 256-bidiStagingEntries; i++ {
		req := routeStreamRequest(fmt.Sprintf("established-%d", i), inferenceSSEPath, false)
		if routes.entries[bidiRouteKey(req, fmt.Sprintf("established-%d", i))] == nil {
			t.Fatal("established route evicted")
		}
	}
}

func TestUnclaimedBidiBytesLeaveCapacityForEstablishedInput(t *testing.T) {
	routes := newBidiRouteRegistry()
	defer routes.close()
	staging := routeAppendRequest("large-unverified", 0, make([]byte, bidiStagingLimit-1024), false)
	_, response := routes.append(staging)
	closeRouteResponse(response)
	if response == nil || response.StatusCode != 200 {
		t.Fatal("staging fixture failed")
	}
	stagedBytes := routes.queuedBytes
	_, response = routes.append(routeAppendRequest("extra-unverified", 0, make([]byte, 2048), false))
	closeRouteResponse(response)
	if response == nil || response.StatusCode != 429 || routes.queuedBytes != stagedBytes {
		t.Fatal("staging byte budget was not enforced")
	}
	// Model an established stream whose replay is in flight, using the rest of
	// the shared byte budget. New authorized input can reclaim staging bytes.
	active := routeAppendRequest("established", 1, []byte{10, 1, 'b'}, false)
	retained := make([]byte, bidiRouteLimit-stagedBytes)
	entry := &bidiRoute{mode: "local", credential: bidiCredential(active), active: 1, touched: time.Now(), pending: map[int64]queuedBidiRequest{0: {body: retained}}}
	routes.entries[bidiRouteKey(active, "established")] = entry
	routes.queuedBytes += len(retained)
	_, response = routes.append(active, func(context.Context, http.Header) bool { return true })
	closeRouteResponse(response)
	if response == nil || response.StatusCode != 200 {
		t.Fatal("staging bytes blocked established input")
	}
	if routes.entries[bidiRouteKey(staging, "large-unverified")] != nil || routes.entries[bidiRouteKey(active, "established")] != entry {
		t.Fatal("wrong route evicted")
	}
	if routes.queuedBytes != len(retained)+len(entry.pending[1].body) {
		t.Fatal("eviction lost byte accounting")
	}
}
