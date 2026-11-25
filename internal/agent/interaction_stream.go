package agent

import (
	"errors"
	"strings"
	"sync"

	aiserverv1 "cursor-byok/internal/protocodec/gen/aiserver/v1"
)

type interactionSubscription struct {
	backgroundComposerID string
	ch                   chan *aiserverv1.InteractionUpdate
}

var interactionSubscribers = struct {
	sync.Mutex
	nextID int
	chans  map[int]interactionSubscription
}{chans: map[int]interactionSubscription{}}

func subscribeInteractionUpdates(backgroundComposerID string) (int, chan *aiserverv1.InteractionUpdate, error) {
	backgroundComposerID = strings.TrimSpace(backgroundComposerID)
	if backgroundComposerID == "" {
		return 0, nil, errors.New("missing bc_id for background interaction stream")
	}
	interactionSubscribers.Lock()
	defer interactionSubscribers.Unlock()
	id := interactionSubscribers.nextID
	interactionSubscribers.nextID++
	ch := make(chan *aiserverv1.InteractionUpdate, 64)
	interactionSubscribers.chans[id] = interactionSubscription{backgroundComposerID: backgroundComposerID, ch: ch}
	return id, ch, nil
}

func unsubscribeInteractionUpdates(id int) {
	interactionSubscribers.Lock()
	subscription := interactionSubscribers.chans[id]
	delete(interactionSubscribers.chans, id)
	interactionSubscribers.Unlock()
	if subscription.ch != nil {
		close(subscription.ch)
	}
}

func publishInteractionUpdate(backgroundComposerID string, msg *aiserverv1.InteractionUpdate) {
	backgroundComposerID = strings.TrimSpace(backgroundComposerID)
	if msg == nil || backgroundComposerID == "" {
		return
	}
	interactionSubscribers.Lock()
	defer interactionSubscribers.Unlock()
	for _, subscription := range interactionSubscribers.chans {
		if subscription.backgroundComposerID != backgroundComposerID {
			continue
		}
		select {
		case subscription.ch <- msg:
		default:
		}
	}
}
