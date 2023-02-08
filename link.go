package amqp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/go-amqp/internal/debug"
	"github.com/Azure/go-amqp/internal/encoding"
	"github.com/Azure/go-amqp/internal/frames"
)

// linkKey uniquely identifies a link on a connection by name and direction.
//
// A link can be identified uniquely by the ordered tuple
//
//	(source-container-id, target-container-id, name)
//
// On a single connection the container ID pairs can be abbreviated
// to a boolean flag indicating the direction of the link.
type linkKey struct {
	name string
	role encoding.Role // Local role: sender/receiver
}

// link contains the common state and methods for sending and receiving links
type link struct {
	key          linkKey               // Name and direction
	handle       uint32                // our handle
	remoteHandle uint32                // remote's handle
	dynamicAddr  bool                  // request a dynamic link address from the server
	rx           chan frames.FrameBody // sessions sends frames for this link on this channel

	// used for gracefully closing link
	close     chan struct{} // signals a link's mux to shut down; DO NOT use this to check if a link has terminated, use done instead
	closeOnce sync.Once     // closeOnce protects close from being closed multiple times

	done    chan struct{} // closed when the link has terminated (mux exited); DO NOT wait on this from within a link's mux() as it will never trigger!
	doneErr error         // contains the error state returned from Close(); DO NOT TOUCH outside of link.go until done has been closed!

	session    *Session                // parent session
	source     *frames.Source          // used for Receiver links
	target     *frames.Target          // used for Sender links
	properties map[encoding.Symbol]any // additional properties sent upon link attach

	// "The delivery-count is initialized by the sender when a link endpoint is created,
	// and is incremented whenever a message is sent. Only the sender MAY independently
	// modify this field. The receiver's value is calculated based on the last known
	// value from the sender and any subsequent messages received on the link. Note that,
	// despite its name, the delivery-count is not a count but a sequence number
	// initialized at an arbitrary point by the sender."
	deliveryCount uint32

	// the currently available credit on the link.
	// for senders, this is the peer's receiving credit.
	// for receivers, this is our receiving credit.
	availableCredit uint32

	senderSettleMode   *SenderSettleMode
	receiverSettleMode *ReceiverSettleMode
	maxMessageSize     uint64
	detachReceived     bool // set to true when the peer initiates link detach/close
}

// attach sends the Attach performative to establish the link with its parent session.
// this is automatically called by the new*Link constructors.
func (l *link) attach(ctx context.Context, beforeAttach func(*frames.PerformAttach), afterAttach func(*frames.PerformAttach)) error {
	if err := l.session.allocateHandle(l); err != nil {
		return err
	}

	attach := &frames.PerformAttach{
		Name:               l.key.name,
		Handle:             l.handle,
		ReceiverSettleMode: l.receiverSettleMode,
		SenderSettleMode:   l.senderSettleMode,
		MaxMessageSize:     l.maxMessageSize,
		Source:             l.source,
		Target:             l.target,
		Properties:         l.properties,
	}

	// link-specific configuration of the attach frame
	beforeAttach(attach)

	_ = l.session.txFrame(attach, nil)

	// wait for response
	var fr frames.FrameBody
	select {
	case <-ctx.Done():
		// attach was written to the network. assume it was received
		// and that the ctx was too short to wait for the ack.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			l.muxClose(ctx, nil, nil, nil)
		}()
		return ctx.Err()
	case <-l.session.done:
		// session has terminated, no need to deallocate in this case
		return l.session.doneErr
	case fr = <-l.rx:
	}
	resp, ok := fr.(*frames.PerformAttach)
	if !ok {
		return fmt.Errorf("unexpected attach response: %#v", fr)
	}

	// If the remote encounters an error during the attach it returns an Attach
	// with no Source or Target. The remote then sends a Detach with an error.
	//
	//   Note that if the application chooses not to create a terminus, the session
	//   endpoint will still create a link endpoint and issue an attach indicating
	//   that the link endpoint has no associated local terminus. In this case, the
	//   session endpoint MUST immediately detach the newly created link endpoint.
	//
	// http://docs.oasis-open.org/amqp/core/v1.0/csprd01/amqp-core-transport-v1.0-csprd01.html#doc-idp386144
	if resp.Source == nil && resp.Target == nil {
		// wait for detach
		select {
		case <-ctx.Done():
			// if we don't send an ack then we're in violation of the protocol
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				l.muxClose(ctx, nil, nil, nil)
			}()
			return ctx.Err()
		case <-l.session.done:
			return l.session.doneErr
		case fr = <-l.rx:
			l.session.deallocateHandle(l)
		}

		detach, ok := fr.(*frames.PerformDetach)
		if !ok {
			return fmt.Errorf("unexpected frame while waiting for detach: %#v", fr)
		}

		// send return detach
		fr = &frames.PerformDetach{
			Handle: l.handle,
			Closed: true,
		}
		_ = l.session.txFrame(fr, nil)

		if detach.Error == nil {
			return fmt.Errorf("received detach with no error specified")
		}
		return detach.Error
	}

	if l.maxMessageSize == 0 || resp.MaxMessageSize < l.maxMessageSize {
		l.maxMessageSize = resp.MaxMessageSize
	}

	// link-specific configuration post attach
	afterAttach(resp)

	if err := l.setSettleModes(resp); err != nil {
		l.muxClose(ctx, nil, nil, nil)
		return err
	}

	return nil
}

// setSettleModes sets the settlement modes based on the resp frames.PerformAttach.
//
// If a settlement mode has been explicitly set locally and it was not honored by the
// server an error is returned.
func (l *link) setSettleModes(resp *frames.PerformAttach) error {
	var (
		localRecvSettle = receiverSettleModeValue(l.receiverSettleMode)
		respRecvSettle  = receiverSettleModeValue(resp.ReceiverSettleMode)
	)
	if l.receiverSettleMode != nil && localRecvSettle != respRecvSettle {
		return fmt.Errorf("amqp: receiver settlement mode %q requested, received %q from server", l.receiverSettleMode, &respRecvSettle)
	}
	l.receiverSettleMode = &respRecvSettle

	var (
		localSendSettle = senderSettleModeValue(l.senderSettleMode)
		respSendSettle  = senderSettleModeValue(resp.SenderSettleMode)
	)
	if l.senderSettleMode != nil && localSendSettle != respSendSettle {
		return fmt.Errorf("amqp: sender settlement mode %q requested, received %q from server", l.senderSettleMode, &respSendSettle)
	}
	l.senderSettleMode = &respSendSettle

	return nil
}

// muxHandleFrame processes fr based on type.
func (l *link) muxHandleFrame(fr frames.FrameBody) error {
	switch fr := fr.(type) {
	// remote side is closing links
	case *frames.PerformDetach:
		// don't currently support link detach and reattach
		if !fr.Closed {
			return &LinkError{inner: fmt.Errorf("non-closing detach not supported: %+v", fr)}
		}

		// set detach received and close link
		l.detachReceived = true

		if fr.Error != nil {
			return &LinkError{RemoteErr: fr.Error}
		}
		return &LinkError{}

	default:
		// TODO: evaluate
		debug.Log(1, "RX (link): unexpected frame: %s", fr)
	}

	return nil
}

// Close closes the Sender and AMQP link.
func (l *link) closeLink(ctx context.Context) error {
	l.closeOnce.Do(func() { close(l.close) })

	select {
	case <-l.done:
		// mux exited
	case <-ctx.Done():
		return ctx.Err()
	}

	var linkErr *LinkError
	if errors.As(l.doneErr, &linkErr) && linkErr.inner == nil {
		// an empty LinkError means the link was closed by the caller
		return nil
	}
	return l.doneErr
}

// muxClose closes the link
//   - err is the error sent to the peer if we're closing the link with an error
//   - deferred is executed during the final phase of shutdown (can be nil)
//   - onRXTransfer handles incoming transfer frames during shutdown (can be nil)
func (l *link) muxClose(ctx context.Context, err *Error, deferred func(), onRXTransfer func(frames.PerformTransfer)) {
	defer func() {
		// final cleanup and signaling

		// if the context timed out or was cancelled we don't really know
		// if the link has been properly terminated.  in this case, it might
		// not be safe to reuse the handle as it might still be associated
		// with an existing link.
		if ctx.Err() == nil {
			// deallocate handle
			l.session.deallocateHandle(l)
		}

		if deferred != nil {
			deferred()
		}

		// signal that the link mux has exited
		close(l.done)
	}()

	// "A peer closes a link by sending the detach frame with the
	// handle for the specified link, and the closed flag set to
	// true. The partner will destroy the corresponding link
	// endpoint, and reply with its own detach frame with the
	// closed flag set to true.
	//
	// Note that one peer MAY send a closing detach while its
	// partner is sending a non-closing detach. In this case,
	// the partner MUST signal that it has closed the link by
	// reattaching and then sending a closing detach."

	fr := &frames.PerformDetach{
		Handle: l.handle,
		Closed: true,
		Error:  err,
	}

Loop:
	for {
		select {
		case <-ctx.Done():
			return
		case l.session.tx <- fr:
			// after sending the detach frame, break the read loop
			break Loop
		case fr := <-l.rx:
			// read from link to avoid blocking session.mux
			switch fr := fr.(type) {
			case *frames.PerformDetach:
				if fr.Closed {
					l.detachReceived = true
				}
			case *frames.PerformTransfer:
				if onRXTransfer != nil {
					onRXTransfer(*fr)
				}
			}
		case <-l.session.done:
			if l.doneErr == nil {
				l.doneErr = l.session.doneErr
			}
			return
		}
	}

	// don't wait for remote to detach when already received
	if l.detachReceived {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return

		// read from link until detach with Close == true is received
		case fr := <-l.rx:
			switch fr := fr.(type) {
			case *frames.PerformDetach:
				if fr.Closed {
					return
				}
			case *frames.PerformTransfer:
				if onRXTransfer != nil {
					onRXTransfer(*fr)
				}
			}

		// connection has ended
		case <-l.session.done:
			if l.doneErr == nil {
				l.doneErr = l.session.doneErr
			}
			return
		}
	}
}
