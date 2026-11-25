package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type inferenceAdmissionWriter struct {
	frames     chan []byte
	endBlocked chan struct{}
	releaseEnd chan struct{}
}

func (w *inferenceAdmissionWriter) Write([]byte) (int, error) {
	return 0, errors.New("expected framed output")
}
func (w *inferenceAdmissionWriter) WriteFrame(body []byte, _ bool) error {
	end, _ := inferenceBytes(body, 4)
	id, _ := inferenceBytes(end, 1)
	if string(id) == "first" {
		close(w.endBlocked)
		<-w.releaseEnd
	}
	w.frames <- append([]byte(nil), body...)
	return nil
}

func TestInferenceAdmissionReleasesBeforeTerminalAndRejectsOnlyExcess(t *testing.T) {
	started := make(chan string, 32)
	firstReady := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request openAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		id, _ := decodeAsString(request.Messages[0].Content)
		started <- id
		if id == "first" {
			select {
			case <-firstReady:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		<-r.Context().Done()
	}))
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w := &inferenceAdmissionWriter{frames: make(chan []byte, 128), endBlocked: make(chan struct{}), releaseEnd: make(chan struct{})}
	messages := make(chan []byte)
	done := make(chan error, 1)
	go func() {
		done <- runInferenceSession(ctx, messages, w, func() []AdapterTarget {
			return []AdapterTarget{{BaseURL: provider.URL, APIKey: "test-key", Model: "test"}}
		})
	}()
	var unblock sync.Once
	defer func() {
		unblock.Do(func() { close(w.releaseEnd) })
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("session workers leaked")
		}
	}()
	messages <- wireBytes(nil, 1, nil)
	invoke := func(id string) {
		request := providerProtoBytes(1, append(providerProtoVarint(1, 1), providerProtoString(2, id)...))
		messages <- wireBytes(nil, 2, wireBytes(wireBytes(nil, 1, []byte(id)), 2, request))
	}
	invoke("first")
	for i := 1; i < 16; i++ {
		invoke(fmt.Sprint(i))
	}
	for i := 0; i < 16; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("initial provider slots not admitted")
		}
	}
	invoke("excess")
	for {
		select {
		case frame := <-w.frames:
			end, _ := inferenceBytes(frame, 4)
			id, _ := inferenceBytes(end, 1)
			if string(id) != "excess" {
				continue
			}
			failure, _ := inferenceBytes(end, 2)
			if providerTestFields(t, failure).varint(1) != 8 {
				t.Fatal("capacity failure not resource_exhausted")
			}
		case <-done:
			t.Fatal("over-capacity invocation canceled the run")
		case <-ctx.Done():
			t.Fatal("capacity failure missing")
		}
		break
	}
	close(firstReady)
	select {
	case <-w.endBlocked:
	case <-ctx.Done():
		t.Fatal("first invocation did not reach terminal frame")
	}
	// A stalled terminal write must apply backpressure to input, not admit
	// an unbounded succession of providers whose end writers all block.
	replacementSent := make(chan struct{})
	go func() { invoke("replacement"); close(replacementSent) }()
	select {
	case <-replacementSent:
		t.Fatal("session accepted more input while terminal output was blocked")
	case <-started:
		t.Fatal("session admitted a provider while terminal output was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	unblock.Do(func() { close(w.releaseEnd) })
	// Once completion is observable, its slot must already be free. A client
	// can replace the invocation immediately without a spurious rejection.
	select {
	case id := <-started:
		if id != "replacement" {
			t.Fatalf("unexpected provider %s", id)
		}
	case <-ctx.Done():
		t.Fatal("completed invocation still occupies admission slot")
	}
}

func TestInferenceHalfCloseBeforeRunRequestFails(t *testing.T) {
	messages := make(chan []byte)
	close(messages)
	err := runInferenceSession(context.Background(), messages, &countingWriter{}, nil)
	if err == nil {
		t.Fatal("input closed before run_request was reported successful")
	}
}
