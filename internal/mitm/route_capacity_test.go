package mitm

import (
	"fmt"
	"testing"
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
