// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jsonrpc2

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/tools/internal/telemetry/event"
)

// Handler is invoked to handle incoming requests.
// If the request returns false from IsNotify then the Handler must eventually
// call Reply on the Conn with the supplied request.
// The handler should return ErrNotHandled if it could not handle the request.
type Handler func(context.Context, *Request) error

// LegacyHooks is a temporary measure during migration from the old Handler
// interface to the new HandleFunc.
// The intent is to delete this interface in a later cl.
type LegacyHooks interface {
	// Deliver is invoked to handle incoming requests.
	// If the request returns false from IsNotify then the Handler must eventually
	// call Reply on the Conn with the supplied request.
	// Handlers are called synchronously, they should pass the work off to a go
	// routine if they are going to take a long time.
	// If Deliver returns true all subsequent handlers will be invoked with
	// delivered set to true, and should not attempt to deliver the message.
	Deliver(ctx context.Context, r *Request, delivered bool) bool

	// Cancel is invoked for cancelled outgoing requests.
	// It is okay to use the connection to send notifications, but the context will
	// be in the cancelled state, so you must do it with the background context
	// instead.
	// If Cancel returns true all subsequent handlers will be invoked with
	// cancelled set to true, and should not attempt to cancel the message.
	Cancel(ctx context.Context, conn *Conn, id ID, cancelled bool) bool

	// Request is called near the start of processing any request.
	Request(ctx context.Context, conn *Conn, direction Direction, r *WireRequest) context.Context
}

// Direction is used to indicate to a logger whether the logged message was being
// sent or received.
type Direction bool

const (
	// Send indicates the message is outgoing.
	Send = Direction(true)
	// Receive indicates the message is incoming.
	Receive = Direction(false)
)

func (d Direction) String() string {
	switch d {
	case Send:
		return "send"
	case Receive:
		return "receive"
	default:
		panic("unreachable")
	}
}

// MethodNotFound is a Handler that replies to all call requests with the
// standard method not found response.
// This should normally be the final handler in a chain.
func MethodNotFound(ctx context.Context, r *Request) error {
	return r.Reply(ctx, nil, NewErrorf(CodeMethodNotFound, "method %q not found", r.Method))
}

// MustReply creates a Handler that panics if the wrapped handler does
// not call Reply for every request that it is passed.
func MustReply(handler Handler) Handler {
	return func(ctx context.Context, req *Request) error {
		err := handler(ctx, req)
		if req.done != nil {
			panic(fmt.Errorf("request %q was never replied to", req.Method))
		}
		return err
	}
}

// CancelHandler returns a handler that supports cancellation, and a canceller
// that can be used to trigger canceling in progress requests.
func CancelHandler(handler Handler) (Handler, Canceller) {
	var mu sync.Mutex
	handling := make(map[ID]context.CancelFunc)
	wrapped := func(ctx context.Context, req *Request) error {
		if req.ID != nil {
			cancelCtx, cancel := context.WithCancel(ctx)
			ctx = cancelCtx
			mu.Lock()
			handling[*req.ID] = cancel
			mu.Unlock()
			req.OnReply(func() {
				mu.Lock()
				delete(handling, *req.ID)
				mu.Unlock()
			})
		}
		return handler(ctx, req)
	}
	return wrapped, func(id ID) {
		mu.Lock()
		cancel, found := handling[id]
		mu.Unlock()
		if found {
			cancel()
		}
	}
}

// AsyncHandler returns a handler that processes each request goes in its own
// goroutine.
// The handler returns immediately, without the request being processed.
// Each request then waits for the previous request to finish before it starts.
// This allows the stream to unblock at the cost of unbounded goroutines
// all stalled on the previous one.
func AsyncHandler(handler Handler) Handler {
	nextRequest := make(chan struct{})
	close(nextRequest)
	return func(ctx context.Context, req *Request) error {
		waitForPrevious := nextRequest
		nextRequest = make(chan struct{})
		unlockNext := nextRequest
		req.OnReply(func() { close(unlockNext) })
		_, queueDone := event.StartSpan(ctx, "queued")
		go func() {
			<-waitForPrevious
			queueDone()
			if err := handler(ctx, req); err != nil {
				event.Error(ctx, "jsonrpc2 async message delivery failed", err)
			}
		}()
		return nil
	}
}

func legacyDeliverHandler(handler Handler) Handler {
	return func(ctx context.Context, req *Request) error {
		if req.conn.LegacyHooks != nil {
			if req.conn.LegacyHooks.Deliver(ctx, req, false) {
				return nil
			}
		}
		return handler(ctx, req)
	}
}

func legacyRequestHandler(handler Handler) Handler {
	return func(ctx context.Context, req *Request) error {
		if req.conn.LegacyHooks != nil {
			ctx = req.conn.LegacyHooks.Request(ctx, req.conn, Receive, &req.WireRequest)
		}
		return handler(ctx, req)
	}
}
