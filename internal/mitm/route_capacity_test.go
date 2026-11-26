package mitm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	queued := existing.pending[0]
	queued.body = make([]byte, bidiStagingLimit)
	existing.pending[0] = queued
	routes.queuedBytes = bidiStagingLimit
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
	routes.dropEntryLocked(bidiRouteKey(req, "existing"))
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
	for i := 0; i < 254; i++ {
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
	if len(routes.entries) != 255+bidiStagingEntries || routes.queuedBytes != before {
		t.Fatal("claim changed acknowledged staging input")
	}
	stream := routeStreamRequest("unverified-0", "/aiserver.v1.AiService/StreamSttSSE", false)
	stream.Header.Set("Authorization", "Bearer unverified-account")
	replays := 0
	_, blocked, lease := routes.claim(stream, "native", false, context.Background(), func(*http.Request) (*http.Response, error) {
		replays++
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	if blocked != nil || lease == nil || replays != 1 {
		t.Fatal("acknowledged staged input was lost after other claims")
	}
	lease.close()
	_, blocked, lease = routes.claim(routeStreamRequest("over-claimed-limit", inferenceSSEPath, false), "local", false, context.Background(), nil)
	if blocked == nil || blocked.StatusCode != 429 || lease != nil {
		t.Fatal("claimed slot limit was not enforced")
	}
	closeRouteResponse(blocked)
	for i := 0; i < 254; i++ {
		req := routeStreamRequest(fmt.Sprintf("established-%d", i), inferenceSSEPath, false)
		if routes.entries[bidiRouteKey(req, fmt.Sprintf("established-%d", i))] == nil {
			t.Fatal("established route evicted")
		}
	}
}

func TestUnclaimedBidiBytesLeaveCapacityForEstablishedInput(t *testing.T) {
	routes := newBidiRouteRegistry()
	defer routes.close()
	staging := routeAppendRequest("large-unverified", 0, nil, false)
	// A 17 MiB payload in the supported hex encoding has a >32 MiB envelope.
	wire := append(wsBytes(2, wsBytes(1, []byte("large-unverified"))), wsBytes(1, []byte(strings.Repeat("00", 17<<20)))...)
	staging.Body = io.NopCloser(bytes.NewReader(wire))
	staging.ContentLength = int64(len(wire))
	_, response := routes.append(staging)
	closeRouteResponse(response)
	if response == nil || response.StatusCode != 200 {
		t.Fatal("staging fixture failed")
	}
	// Preserve the previous per-request limit: >32 MiB was accepted above.
	staged := routes.entries[bidiRouteKey(staging, "large-unverified")]
	staged.pending[1] = queuedBidiRequest{body: make([]byte, bidiStagingLimit-routes.queuedBytes)}
	routes.queuedBytes = bidiStagingLimit
	stagedBytes := routes.queuedBytes
	_, response = routes.append(routeAppendRequest("extra-unverified", 0, make([]byte, 2048), false))
	closeRouteResponse(response)
	if response == nil || response.StatusCode != 429 || routes.queuedBytes != stagedBytes {
		t.Fatal("staging byte budget was not enforced")
	}
	// Model an established replay near its separate budget. Authorized input
	// remains admissible without discarding any acknowledged staged input.
	active := routeAppendRequest("established", 1, []byte{10, 1, 'b'}, false)
	retained := make([]byte, bidiRouteLimit-1024)
	entry := &bidiRoute{mode: "local", credential: bidiCredential(active), active: 1, touched: time.Now(), pending: map[int64]queuedBidiRequest{0: {body: retained}}}
	routes.entries[bidiRouteKey(active, "established")] = entry
	routes.queuedBytes += len(retained)
	_, response = routes.append(active, func(context.Context, http.Header) bool { return true })
	closeRouteResponse(response)
	if response == nil || response.StatusCode != 200 {
		t.Fatal("staging bytes blocked established input")
	}
	if routes.entries[bidiRouteKey(staging, "large-unverified")] != staged || routes.entries[bidiRouteKey(active, "established")] != entry {
		t.Fatal("acknowledged input was evicted")
	}
	if routes.queuedBytes != stagedBytes+len(retained)+len(entry.pending[1].body) {
		t.Fatal("separate budgets lost byte accounting")
	}
}

func TestBidiPromotionRetainsInputWhenClaimedBudgetIsFull(t *testing.T) {
	routes := newBidiRouteRegistry()
	defer routes.close()
	request := routeAppendRequest("promotion", 0, []byte{10, 1, 'x'}, false)
	_, response := routes.append(request)
	closeRouteResponse(response)
	stagedBytes := routes.queuedBytes
	routes.entries["full-claimed-fixture"] = &bidiRoute{mode: "native", active: 1, touched: time.Now(), pending: map[int64]queuedBidiRequest{0: {body: make([]byte, bidiRouteLimit)}}}
	routes.queuedBytes += bidiRouteLimit
	stream := func() *http.Request {
		return routeStreamRequest("promotion", "/aiserver.v1.AiService/StreamSttSSE", false)
	}
	_, blocked, lease := routes.claim(stream(), "native", false, context.Background(), nil)
	if blocked == nil || blocked.StatusCode != 429 || lease != nil || routes.queuedBytes != bidiRouteLimit+stagedBytes {
		t.Fatal("full claimed budget lost staged input")
	}
	closeRouteResponse(blocked)
	routes.dropEntryLocked("full-claimed-fixture")
	replays := 0
	_, blocked, lease = routes.claim(stream(), "native", false, context.Background(), func(*http.Request) (*http.Response, error) {
		replays++
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	if blocked != nil || lease == nil || replays != 1 || routes.queuedBytes != 0 {
		t.Fatal("retry did not replay retained input")
	}
	lease.close()
}
