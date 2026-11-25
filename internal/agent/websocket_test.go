package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func TestDecodeSocketFrameRejectsMalformedInput(t *testing.T) {
	for name, frame := range map[string][]byte{
		"empty":                 nil,
		"truncated":             {0x0a, 0x02, 0x08},
		"unknown version":       {0x0a, 0x02, 0x08, 0x02},
		"wrong hello wire type": {0x08, 0x01},
		"duplicate oneof":       {0x0a, 0x02, 0x08, 0x01, 0x0a, 0x02, 0x08, 0x01},
		"empty identity":        {0x12, 0x02, 0x0a, 0x00},
		"missing payload":       {0x1a, 0x03, 0x0a, 0x01, 'r'},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeSocketFrame(frame); err == nil {
				t.Fatalf("accepted malformed %x", frame)
			}
		})
	}
}

type countingWriter struct {
	bytes.Buffer
	writes int
}

func (w *countingWriter) Write(b []byte) (int, error) { w.writes++; return w.Buffer.Write(b) }

func TestLegacyStreamErrorIsOneCompleteConnectFrame(t *testing.T) {
	var sink countingWriter
	writeEndStreamError(&lockedWriter{w: &sink}, "provider failed")
	if sink.writes != 1 {
		t.Fatalf("terminal frame used %d writes, permits keepalive interleaving", sink.writes)
	}
	b := sink.Bytes()
	if b[0]&flagEndStream == 0 || int(binary.BigEndian.Uint32(b[1:5])) != len(b)-5 {
		t.Fatalf("invalid Connect frame %x", b)
	}
	body := b[5:]
	if b[0]&flagCompressed != 0 {
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		body, err = io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	var decoded struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Error.Message != "provider failed" {
		t.Fatalf("error frame = %s (%v)", body, err)
	}
}

func TestCanceledToolAndInteractionWaitsReleasePendingEntries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := registerToolWait("cancelled-owner", "cancelled-test-tool")
	registerExecIDAlias("cancelled-owner", "cancelled-test-exec", 123456, "cancelled-test-tool")
	result := waitForToolResultContext(ctx, tool, time.Hour)
	if result.Error != context.Canceled.Error() {
		t.Fatalf("result = %+v", result)
	}
	interaction := registerInteractionWait("cancelled-owner", 123456)
	if r := waitForInteractionResponse(ctx, interaction, time.Hour); r != nil {
		t.Fatal("cancel returned a verdict")
	}
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if pending[toolWaitKey{"cancelled-owner", "cancelled-test-tool"}] != nil || execIDAlias[toolWaitKey{"cancelled-owner", "cancelled-test-exec"}] != "" || seqAlias[sequenceWaitKey{"cancelled-owner", 123456}] != "" || pendingInteraction[sequenceWaitKey{"cancelled-owner", 123456}] != nil {
		t.Fatal("cancel retained wait state")
	}
}
