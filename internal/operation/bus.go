package operation

import (
	"sync"

	dcctlv1 "github.com/webitel/webitel-dcctl/gen/go/dcctl/v1"
)

// subscriptionBuffer is how many changes a subscription holds unread.
const subscriptionBuffer = 64

// bus passes every saved change of an operation to its subscribers.
type bus struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

func newBus() *bus {
	return &bus{subs: make(map[*Subscription]struct{})}
}

// Subscription receives the changes of an operation, each as the operation
// after the change, in the order of their revisions. The operations are shared
// by the subscribers and must not be modified.
//
// A subscriber that leaves [subscriptionBuffer] changes unread is dropped:
// its channel is closed without [Subscription.Close], and it reads the
// operation again and subscribes anew. The engine never waits for a
// subscriber.
type Subscription struct {
	name string
	c    chan *dcctlv1.Operation
	bus  *bus
}

// C returns the channel of the changes.
func (s *Subscription) C() <-chan *dcctlv1.Operation { return s.c }

// Close ends the subscription and closes its channel. A later call does
// nothing.
func (s *Subscription) Close() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()

	s.bus.drop(s)
}

func (b *bus) subscribe(name string) *Subscription {
	s := &Subscription{name: name, c: make(chan *dcctlv1.Operation, subscriptionBuffer), bus: b}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.subs[s] = struct{}{}

	return s
}

// publish passes op to the subscribers of its name.
func (b *bus) publish(op *dcctlv1.Operation) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for s := range b.subs {
		if s.name != "" && s.name != op.GetName() {
			continue
		}

		select {
		case s.c <- op:
		default:
			b.drop(s)
		}
	}
}

// drop removes s and closes its channel; b.mu is held.
func (b *bus) drop(s *Subscription) {
	if _, ok := b.subs[s]; !ok {
		return
	}

	delete(b.subs, s)
	close(s.c)
}
