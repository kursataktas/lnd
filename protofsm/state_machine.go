package protofsm

import (
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// pollInterval is the interval at which we'll poll the SendWhen
	// predicate if specified.
	pollInterval = time.Millisecond * 100
)

// EmittedEvent is a special type that can be emitted by a state transition.
// This can container internal events which are to be routed back to the state,
// or external events which are to be sent to the daemon.
type EmittedEvent[Event any] struct {
	// InternalEvent is an optional internal event that is to be routed
	// back to the target state. This enables state to trigger one or many
	// state transitions without a new external event.
	InternalEvent fn.Option[Event]

	// ExternalEvent is an optional external event that is to be sent to
	// the daemon for dispatch. Usually, this is some form of I/O.
	ExternalEvents fn.Option[DaemonEventSet]
}

// StateTransition is a state transition type. It denotes the next state to go
// to, and also the set of events to emit.
type StateTransition[Event any, Env Environment] struct {
	// NextState is the next state to transition to.
	NextState State[Event, Env]

	// NewEvents is the set of events to emit.
	NewEvents fn.Option[EmittedEvent[Event]]
}

// Environment is an abstract interface that represents the environment that
// the state machine will execute using. From the PoV of the main state machine
// executor, we just care about being able to clean up any resources that were
// allocated by the environment.
type Environment interface {
	// CleanUp is a method that'll be called once the state machine has
	// reached a terminal state. It signals the end of the execution of the
	// state machine.
	CleanUp() error

	// TODO(roasbeef): also add checkpointing?
}

// State defines an abstract state along, namely its state transition function
// that takes as input an event and an environment, and returns a state
// transition (next state, and set of events to emit). As state can also either
// be terminal, or not, a terminal event causes state execution to halt.
type State[Event any, Env Environment] interface {
	// ProcessEvent takes an event and an environment, and returns a new
	// state transition. This will be iteratively called until either a
	// terminal state is reached, or no further internal events are
	// emitted.
	ProcessEvent(event Event, env Env) (*StateTransition[Event, Env], error)

	// IsTerminal returns true if this state is terminal, and false otherwise.
	IsTerminal() bool

	// TODO(roasbeef): also add state serialization?
}

// DaemonAdapters is a set of methods that server as adapters to bridge the
// pure world of the FSM to the real world of the daemon. These will be used to
// do things like broadcast transactions, or send messages to peers.
type DaemonAdapters interface {
	// SendMessages sends the target set of messages to the target peer.
	SendMessages(btcec.PublicKey, []lnwire.Message) error

	// BroadcastTransaction broadcasts a transaction with the target label.
	BroadcastTransaction(*wire.MsgTx, string) error

	// RegisterConfirmationsNtfn registers an intent to be notified once
	// txid reaches numConfs confirmations. We also pass in the pkScript as
	// the default light client instead needs to match on scripts created
	// in the block. If a nil txid is passed in, then not only should we
	// match on the script, but we should also dispatch once the
	// transaction containing the script reaches numConfs confirmations.
	// This can be useful in instances where we only know the script in
	// advance, but not the transaction containing it.
	//
	// TODO(roasbeef): could abstract further?
	RegisterConfirmationsNtfn(txid *chainhash.Hash, pkScript []byte,
		numConfs, heightHint uint32,
		opts ...chainntnfs.NotifierOption,
	) (*chainntnfs.ConfirmationEvent, error)

	// RegisterSpendNtfn registers an intent to be notified once the target
	// outpoint is successfully spent within a transaction. The script that
	// the outpoint creates must also be specified. This allows this
	// interface to be implemented by BIP 158-like filtering.
	RegisterSpendNtfn(outpoint *wire.OutPoint, pkScript []byte,
		heightHint uint32) (*chainntnfs.SpendEvent, error)
}

// stateQuery is used by outside callers to query the internal state of the
// state machine.
type stateQuery[Event any, Env Environment] struct {
	// CurrentState is a channel that will be sent the current state of the
	// state machine.
	CurrentState chan State[Event, Env]
}

// StateMachine represents an abstract FSM that is able to process new incoming
// events and drive a state machine to termination. This implementation uses
// type params to abstract over the types of events and environment. Events
// trigger new state transitions, that use the environment to perform some
// action.
//
// TODO(roasbeef): terminal check, daemon event execution, init?
type StateMachine[Event any, Env Environment] struct {
	currentState State[Event, Env]
	env          Env

	daemon DaemonAdapters

	events chan Event

	quit chan struct{}
	wg   sync.WaitGroup

	// newStateEvents is an EventDistributor that will be used to notify
	// any relevant callers of new state transitions that occur.
	newStateEvents *fn.EventDistributor[State[Event, Env]]

	stateQuery chan stateQuery[Event, Env]

	startOnce sync.Once
	stopOnce  sync.Once

	// TODO(roasbeef): also use that context guard here?
}

// NewStateMachine creates a new state machine given a set of daemon adapters,
// an initial state, and an environment.
func NewStateMachine[Event any, Env Environment](adapters DaemonAdapters,
	initialState State[Event, Env],
	env Env) StateMachine[Event, Env] {

	return StateMachine[Event, Env]{
		daemon:         adapters,
		events:         make(chan Event, 1),
		currentState:   initialState,
		stateQuery:     make(chan stateQuery[Event, Env]),
		quit:           make(chan struct{}),
		env:            env,
		newStateEvents: fn.NewEventDistributor[State[Event, Env]](),
	}
}

// Start starts the state machine. This will spawn a goroutine that will drive
// the state machine to completion.
func (s *StateMachine[Event, Env]) Start() {
	s.startOnce.Do(func() {
		s.wg.Add(1)
		go s.driveMachine()
	})
}

// Stop stops the state machine. This will block until the state machine has
// reached a stopping point.
func (s *StateMachine[Event, Env]) Stop() {
	s.stopOnce.Do(func() {
		close(s.quit)
		s.wg.Wait()
	})
}

// SendEvent sends a new event to the state machine.
//
// TODO(roasbeef): bool if processed?
func (s *StateMachine[Event, Env]) SendEvent(event Event) {
	select {
	case s.events <- event:
	case <-s.quit:
		return
	}
}

// CurrentState returns the current state of the state machine.
func (s *StateMachine[Event, Env]) CurrentState() (State[Event, Env], error) {
	query := stateQuery[Event, Env]{
		CurrentState: make(chan State[Event, Env], 1),
	}

	if !fn.SendOrQuit(s.stateQuery, query, s.quit) {
		return nil, fmt.Errorf("state machine is shutting down")
	}

	return fn.RecvOrTimeout(query.CurrentState, time.Second)
}

// StateSubscriber represents an active subscription to be notified of new
// state transitions.
type StateSubscriber[E any, F Environment] *fn.EventReceiver[State[E, F]]

// RegisterStateEvents registers a new event listener that will be notified of
// new state transitions.
func (s *StateMachine[Event, Env]) RegisterStateEvents() StateSubscriber[Event, Env] {
	subscriber := fn.NewEventReceiver[State[Event, Env]](10)

	// TODO(roasbeef): instead give the state and the input event?

	s.newStateEvents.RegisterSubscriber(subscriber)

	return subscriber
}

// RemoveStateSub removes the target state subscriber from the set of active
// subscribers.
func (s *StateMachine[Event, Env]) RemoveStateSub(sub StateSubscriber[Event, Env]) {
	s.newStateEvents.RemoveSubscriber(sub)
}

// executeDaemonEvent executes a daemon event, which is a special type of event
// that can be emitted as part of the state transition function of the state
// machine. An error is returned if the type of event is unknown.
func (s *StateMachine[Event, Env]) executeDaemonEvent(event DaemonEvent) error {
	switch daemonEvent := event.(type) {

	// This is a send message event, so we'll send the event, and also mind
	// any preconditions as well as post-send events.
	case *SendMsgEvent[Event]:
		sendAndCleanUp := func() error {
			err := s.daemon.SendMessages(
				daemonEvent.TargetPeer, daemonEvent.Msgs,
			)
			if err != nil {
				return fmt.Errorf("unable to send msgs: %w", err)
			}

			// If a post-send event was specified, then we'll
			// funnel that back into the main state machine now as
			// well.
			daemonEvent.PostSendEvent.WhenSome(func(event Event) {
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()

					s.SendEvent(event)
				}()
			})

			return nil
		}

		// If this doesn't have a SendWhen predicate, then we can just
		// send it off right away.
		if !daemonEvent.SendWhen.IsSome() {
			return sendAndCleanUp()
		}

		// Otherwise, this has a SendWhen predicate, so we'll need
		// launch a goroutine to poll the SendWhen, then send only once
		// the predicate is true.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()

			predicateTicker := time.NewTicker(pollInterval)
			defer predicateTicker.Stop()

			for {
				select {
				case <-predicateTicker.C:
					canSend := fn.MapOptionZ(
						daemonEvent.SendWhen,
						func(pred SendPredicate) bool {
							return pred()
						},
					)

					if canSend {
						sendAndCleanUp()
						return
					}

				case <-s.quit:
					return
				}
			}
		}()

		return nil

	// If this is a broadcast transaction event, then we'll broadcast with
	// the label attached.
	case *BroadcastTxn:
		err := s.daemon.BroadcastTransaction(
			daemonEvent.Tx, daemonEvent.Label,
		)
		if err != nil {
			// TODO(roasbeef): hook has channel read event event is
			// hit?
			return fmt.Errorf("unable to broadcast txn: %w", err)
		}

		return nil

	// The state machine has requested a new event to be sent once a
	// transaction spending a specified outpoint has confirmed.
	case *RegisterSpend[Event]:
		spendEvent, err := s.daemon.RegisterSpendNtfn(
			&daemonEvent.OutPoint, daemonEvent.PkScript,
			daemonEvent.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to register spend: %w", err)
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case <-spendEvent.Spend:
					// If there's a post-send event, then
					// we'll send that into the current
					// state now.
					postSpend := daemonEvent.PostSpendEvent
					postSpend.WhenSome(func(e Event) {
						s.SendEvent(e)
					})

					return

				case <-s.quit:
					return
				}
			}
		}()

		return nil

	// The state machine has requested a new event to be sent once a
	// specified txid+pkScript pair has confirmed.
	case *RegisterConf[Event]:
		numConfs := daemonEvent.NumConfs.UnwrapOr(1)
		confEvent, err := s.daemon.RegisterConfirmationsNtfn(
			&daemonEvent.Txid, daemonEvent.PkScript,
			numConfs, daemonEvent.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to register conf: %w", err)
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case <-confEvent.Confirmed:
					// If there's a post-conf event, then
					// we'll send that into the current
					// state now.
					//
					// TODO(roasbeef): refactor to
					// dispatchAfterRecv w/ above
					postConf := daemonEvent.PostConfEvent
					postConf.WhenSome(func(e Event) {
						s.SendEvent(e)
					})

					return

				case <-s.quit:
					return
				}
			}
		}()
	}

	return fmt.Errorf("unknown daemon event: %T", event)
}

// applyEvents applies a new event to the state machine. This will continue
// until no further events are emitted by the state machine. Along the way,
// we'll also ensure to execute any daemon events that are emitted.
func (s *StateMachine[Event, Env]) applyEvents(newEvent Event) (State[Event, Env], error) {
	// TODO(roasbeef): make starting state as part of env?
	currentState := s.currentState

	eventQueue := fn.NewQueue(newEvent)

	// Given the next event to handle, we'll process the event, then add
	// any new emitted internal events to our event queue. This continues
	// until we reach a terminal state, or we run out of internal events to
	// process.
	for nextEvent := eventQueue.Dequeue(); nextEvent.IsSome(); nextEvent = eventQueue.Dequeue() {
		err := fn.MapOptionZ(nextEvent, func(event Event) error {
			// Apply the state transition function of the current
			// state given this new event and our existing env.
			transition, err := currentState.ProcessEvent(
				event, s.env,
			)
			if err != nil {
				return err
			}

			newEvents := transition.NewEvents
			err = fn.MapOptionZ(newEvents, func(events EmittedEvent[Event]) error {
				// With the event processed, we'll process any
				// new daemon events that were emitted as part
				// of this new state transition.
				err := fn.MapOptionZ(events.ExternalEvents, func(dEvents DaemonEventSet) error {
					for _, dEvent := range dEvents {
						err := s.executeDaemonEvent(dEvent)
						if err != nil {
							return err
						}
					}

					return nil
				})
				if err != nil {
					return err
				}

				// Next, we'll add any new emitted events to
				// our event queue.
				events.InternalEvent.WhenSome(func(inEvent Event) {
					eventQueue.Enqueue(inEvent)
				})

				return nil
			})
			if err != nil {
				return err
			}

			// With our events processed, we'll now update our
			// internal state.
			currentState = transition.NextState

			// Notify our subscribers of the new state transition.
			s.newStateEvents.NotifySubscribers(currentState)

			return nil
		})
		if err != nil {
			return currentState, err
		}
	}

	return currentState, nil
}

// driveMachine is the main event loop of the state machine. It accepts any new
// incoming events, and then drives the state machine forward until it reaches
// a terminal state.
func (s *StateMachine[Event, Env]) driveMachine() {
	defer s.wg.Done()

	// TODO(roasbeef): move into env? read only to start with
	currentState := s.currentState

	// We just started driving the state machine, so we'll notify our
	// subscribers of this starting state.
	s.newStateEvents.NotifySubscribers(currentState)

	for {
		select {
		// We have a new external event, so we'll drive the state
		// machine forward until we either run out of internal events,
		// or we reach a terminal state.
		case newEvent := <-s.events:
			newState, err := s.applyEvents(newEvent)
			if err != nil {
				// TODO(roasbeef): hard error?
				log.Errorf("unable to apply event: %v", err)
				continue
			}

			currentState = newState

			// If this is a terminal event, then we'll exit the
			// state machine and call any relevant clean up call
			// backs that might have been registered.
			if currentState.IsTerminal() {
				err := s.env.CleanUp()
				if err != nil {
					log.Errorf("unable to clean up "+
						"env: %v", err)
				}
			}

		// An outside caller is querying our state, so we'll return the
		// latest state.
		case stateQuery := <-s.stateQuery:
			if !fn.SendOrQuit(stateQuery.CurrentState, currentState, s.quit) {
				return
			}

		case <-s.quit:
			// TODO(roasbeef): logs, etc
			//  * something in env?
			return
		}
	}
}
