// Package notify sends notifications about agent sessions and forges. It
// subscribes to the event bus, decides what notifies (notifications.on),
// rate-limits and de-duplicates per session (notifications.debounce), and
// publishes to ntfy.
//
// Nothing here can hold up the bus: events arrive through a bounded
// subscription, and messages are published from a separate goroutine
// through a bounded queue.
package notify

import (
	"context"
	"net/http"
	"sync"
	"time"

	"charm.land/log/v2"
	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/events"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// queueSize is how many messages can wait for the publisher. It is only
// reached when ntfy is unreachable for a long time and many sessions change
// at once; messages beyond it are dropped and counted.
const queueSize = 64

var notificationsCounter = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "sundae",
	Subsystem: "notifications",
	Name:      "total",
	Help:      "Notifications by sink and result: sent, failed (rejected, or retries exhausted) or dropped (queue full).",
}, []string{"sink", "result"})

// Clock is the notifier's time source. Tests replace it to drive debouncing
// without sleeping.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is a stoppable one-shot timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.t.C }
func (t realTimer) Stop() bool          { return t.t.Stop() }

// Option configures a Notifier.
type Option func(*Notifier)

// WithLogger sets the logger.
func WithLogger(l *log.Logger) Option {
	return func(n *Notifier) { n.logger = l }
}

// WithClock sets the clock used for debouncing.
func WithClock(c Clock) Option {
	return func(n *Notifier) { n.clock = c }
}

// WithHTTPClient sets the HTTP client used to reach ntfy.
func WithHTTPClient(c *http.Client) Option {
	return func(n *Notifier) { n.httpClient = c }
}

// WithRetry sets the timeout of one publish request and how long one
// message is retried for.
func WithRetry(attemptTimeout, retryFor time.Duration) Option {
	return func(n *Notifier) {
		n.attemptTimeout = attemptTimeout
		n.retryFor = retryFor
	}
}

// Notifier turns bus events into ntfy notifications.
type Notifier struct {
	logger         *log.Logger
	clock          Clock
	httpClient     *http.Client
	attemptTimeout time.Duration
	retryFor       time.Duration

	classifier *classifier
	debouncer  *debouncer
	ntfy       *ntfyPublisher
	// publish sends one message; it is ntfy.Publish outside tests.
	publish func(context.Context, Message) error

	queue      chan Message
	stop       chan struct{}
	loopDone   chan struct{}
	sendDone   chan struct{}
	sendCtx    context.Context
	sendCancel context.CancelFunc
	startOnce  sync.Once
	stopOnce   sync.Once
	started    bool
}

// New returns a notifier for the notifications section of a validated
// config. It does nothing until Start.
func New(nc config.NotificationsConfig, opts ...Option) *Notifier {
	n := &Notifier{
		clock:          realClock{},
		attemptTimeout: DefaultAttemptTimeout,
		retryFor:       DefaultRetryFor,
		classifier:     newClassifier(nc),
		debouncer:      newDebouncer(nc.Debounce),
		queue:          make(chan Message, queueSize),
		stop:           make(chan struct{}),
		loopDone:       make(chan struct{}),
		sendDone:       make(chan struct{}),
	}
	for _, o := range opts {
		o(n)
	}
	if n.logger == nil {
		n.logger = log.Default()
	}
	n.logger = n.logger.WithPrefix("notify")
	n.ntfy = newNtfyPublisher(nc.Ntfy, n.httpClient)
	n.ntfy.attemptTimeout = n.attemptTimeout
	n.ntfy.retryFor = n.retryFor
	n.publish = n.ntfy.Publish
	n.sendCtx, n.sendCancel = context.WithCancel(context.Background())
	return n
}

// Start subscribes to bus and starts publishing. Calling it again does
// nothing.
func (n *Notifier) Start(bus *events.Bus) {
	n.startOnce.Do(func() {
		n.started = true
		sub := bus.Subscribe(events.Named("ntfy"))
		go n.loop(sub)
		go n.sender()
	})
}

// Close stops taking events, sends what the debouncer still holds, and
// waits for queued messages to be published until ctx is done, when it
// abandons the rest. It is safe to call more than once.
func (n *Notifier) Close(ctx context.Context) error {
	if n == nil {
		return nil
	}
	n.stopOnce.Do(func() { close(n.stop) })
	n.startOnce.Do(func() {}) // a notifier never started has nothing to wait for
	if !n.started {
		n.sendCancel()
		return nil
	}
	<-n.loopDone
	select {
	case <-n.sendDone:
		n.sendCancel()
		return nil
	case <-ctx.Done():
		n.sendCancel()
		<-n.sendDone
		return ctx.Err()
	}
}

// loop feeds bus events through the classifier and the debouncer. It owns
// both. It ends when Close is called or the bus is closed.
func (n *Notifier) loop(sub *events.Subscription) {
	defer close(n.loopDone)
	defer close(n.queue)
	defer sub.Cancel()

	var timer Timer
	var timerC <-chan time.Time
	rearm := func() {
		if timer != nil {
			timer.Stop()
			timer, timerC = nil, nil
		}
		if at, ok := n.debouncer.next(); ok {
			timer = n.clock.NewTimer(max(at.Sub(n.clock.Now()), 0))
			timerC = timer.C()
		}
	}
	flush := func() {
		for _, out := range n.debouncer.flush() {
			n.enqueue(out.msg)
		}
		if timer != nil {
			timer.Stop()
		}
	}

	handle := func(ev events.Event) {
		if obs, ok := n.classifier.classify(ev); ok {
			for _, out := range n.debouncer.observe(obs, n.clock.Now()) {
				n.enqueue(out.msg)
			}
			rearm()
		}
	}

	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				flush()
				return
			}
			handle(ev)
		case <-timerC:
			for _, out := range n.debouncer.due(n.clock.Now()) {
				n.enqueue(out.msg)
			}
			rearm()
		case <-n.stop:
			// Take what was published before Close, then send what is
			// held.
			sub.Cancel()
			for ev := range sub.C {
				handle(ev)
			}
			flush()
			return
		}
	}
}

// enqueue hands m to the sender without blocking.
func (n *Notifier) enqueue(m Message) {
	select {
	case n.queue <- m:
	default:
		notificationsCounter.WithLabelValues("ntfy", "dropped").Inc()
		n.logger.Warn("notification queue full; dropping", "title", m.Title)
	}
}

// sender publishes queued messages one at a time.
func (n *Notifier) sender() {
	defer close(n.sendDone)
	for m := range n.queue {
		if n.sendCtx.Err() != nil {
			notificationsCounter.WithLabelValues("ntfy", "dropped").Inc()
			continue
		}
		if err := n.publish(n.sendCtx, m); err != nil {
			notificationsCounter.WithLabelValues("ntfy", "failed").Inc()
			n.logger.Warn("ntfy publish failed", "title", m.Title, "err", err)
			continue
		}
		notificationsCounter.WithLabelValues("ntfy", "sent").Inc()
		n.logger.Debug("ntfy notification sent", "title", m.Title)
	}
}
