package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
	"google.golang.org/protobuf/proto"
)

func interactionOutput(text string) *aiserverv1.InteractionUpdate {
	return &aiserverv1.InteractionUpdate{Message: &aiserverv1.InteractionUpdate_ShellOutputDelta{ShellOutputDelta: &aiserverv1.ShellOutputDeltaUpdate{Event: &aiserverv1.ShellOutputDeltaUpdate_Stdout{Stdout: &aiserverv1.ShellStreamStdout{Data: text}}}}}
}

func interactionSubscriptionForTest(t *testing.T, bcID string) chan *aiserverv1.InteractionUpdate {
	t.Helper()
	id, ch, err := subscribeInteractionUpdates(bcID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unsubscribeInteractionUpdates(id) })
	return ch
}

func noInteraction(t *testing.T, ch <-chan *aiserverv1.InteractionUpdate) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("unrelated interaction delivered: %v", msg)
	default:
	}
}

func TestBackgroundInteractionSubscribersAreIsolated(t *testing.T) {
	firstID, secondID := t.Name()+"-first", t.Name()+"-second"
	first := interactionSubscriptionForTest(t, firstID)
	mirror := interactionSubscriptionForTest(t, firstID)
	second := interactionSubscriptionForTest(t, secondID)
	publishInteractionUpdate(firstID, interactionOutput("first"))
	for _, ch := range []chan *aiserverv1.InteractionUpdate{first, mirror} {
		select {
		case msg := <-ch:
			if msg.GetShellOutputDelta().GetStdout().GetData() != "first" {
				t.Fatal("wrong interaction")
			}
		default:
			t.Fatal("owner interaction missing")
		}
	}
	noInteraction(t, second)
	publishInteractionUpdate("", interactionOutput("unscoped"))
	publishInteractionUpdate(t.Name()+"-unknown", interactionOutput("unknown"))
	noInteraction(t, first)
	noInteraction(t, second)
	if _, _, err := subscribeInteractionUpdates(" \t"); err == nil {
		t.Fatal("accepted an unscoped subscription")
	}
}

func TestShellPublishingRequiresExplicitBackgroundWaiter(t *testing.T) {
	bcID := t.Name()
	stream := interactionSubscriptionForTest(t, bcID)
	other := interactionSubscriptionForTest(t, bcID+"-other")
	for i, scope := range []toolWaitScope{bidiToolScope(bcID), socketToolScope(bcID)} {
		wait := registerToolWait(scope, "foreground")
		registerExecIDAlias(scope, "foreground-exec", uint32(i+1), "foreground")
		routeExecClientResult(scope, shellOutput(uint32(i+1), "foreground output"))
		cancelToolWait(wait)
		noInteraction(t, stream)
	}
	scope := bidiToolScope(bcID)
	wait := registerBackgroundToolWait(scope, "background", bcID)
	defer cancelToolWait(wait)
	registerExecIDAlias(scope, "background-exec", 10, "background")
	routeExecClientResult(scope, shellOutput(10, "owned output"))
	select {
	case msg := <-stream:
		if msg.GetShellOutputDelta().GetStdout().GetData() != "owned output" {
			t.Fatal("wrong background output")
		}
	default:
		t.Fatal("background output missing")
	}
	noInteraction(t, other)
	cancelToolWait(wait)
	routeExecClientResult(scope, shellOutput(10, "late output"))
	noInteraction(t, stream)
}

func decodeInteractionWire(t *testing.T, frame []byte) (byte, []byte) {
	t.Helper()
	if len(frame) < 5 || int(binary.BigEndian.Uint32(frame[1:5])) != len(frame)-5 {
		t.Fatalf("invalid interaction frame length: %d", len(frame))
	}
	body := frame[5:]
	if frame[0]&flagCompressed != 0 {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		body, err = io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	return frame[0], body
}

func TestMissingBackgroundStreamIDReturnsExplicitError(t *testing.T) {
	var output bytes.Buffer
	HandleBackgroundComposerInteractionUpdates(context.Background(), nil, "application/proto", &output)
	flags, body := decodeInteractionWire(t, output.Bytes())
	if flags&flagEndStream == 0 || !strings.Contains(string(body), "missing bc_id") {
		t.Fatalf("missing scope did not fail closed: %s", body)
	}
}

type interactionFrameSink chan []byte

func (sink interactionFrameSink) Write(data []byte) (int, error) {
	sink <- bytes.Clone(data)
	return len(data), nil
}

func TestInteractionHandlerUsesRequestBCIDOnWire(t *testing.T) {
	bcID := t.Name()
	request, err := proto.Marshal(&aiserverv1.StreamInteractionUpdatesRequest{BcId: bcID})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := make(interactionFrameSink, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		HandleBackgroundComposerInteractionUpdates(ctx, request, "application/proto", sink)
	}()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			publishInteractionUpdate(bcID+"-other", interactionOutput("wrong conversation"))
			publishInteractionUpdate(bcID, interactionOutput("owner"))
		case frame := <-sink:
			_, body := decodeInteractionWire(t, frame)
			msg := &aiserverv1.InteractionUpdate{}
			if err := proto.Unmarshal(body, msg); err != nil {
				t.Fatal(err)
			}
			if msg.GetShellOutputDelta().GetStdout().GetData() != "owner" {
				t.Fatalf("wire stream crossed conversation: %v", msg)
			}
			cancel()
			select {
			case <-done:
			case <-deadline.C:
				t.Fatal("stream did not stop on cancellation")
			}
			return
		case <-deadline.C:
			t.Fatal("owner update never reached its wire stream")
		}
	}
}

func TestConcurrentInteractionPublishingAndUnsubscribe(t *testing.T) {
	const workers = 32
	bcID := t.Name()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, err := subscribeInteractionUpdates(bcID)
			if err != nil {
				t.Error(err)
				return
			}
			publishInteractionUpdate(bcID, interactionOutput("test"))
			unsubscribeInteractionUpdates(id)
		}()
	}
	wg.Wait()
}
