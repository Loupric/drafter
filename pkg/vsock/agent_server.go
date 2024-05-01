package vsock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/loopholelabs/drafter/pkg/remotes"
	"github.com/loopholelabs/drafter/pkg/utils"
	"github.com/pojntfx/panrpc/go/pkg/rpc"
)

var (
	ErrNoRemoteFound = errors.New("no remote found")

	ErrAgentClientDisconnected = errors.New("agent client disconnected")
	ErrAgentClientAcceptFailed = errors.New("agent client accept failed")
)

type AgentServer struct {
	VSockPath string

	Accept func(
		acceptCtx context.Context,
		remoteCtx context.Context,
	) (
		acceptingAgent *AcceptingAgentServer,

		errs error,
	)

	Close func()
}

type AcceptingAgentServer struct {
	Remote remotes.AgentRemote

	Wait  func() error
	Close func() error
}

func StartAgentServer(
	vsockPath string,
	vsockPort uint32,
) (
	agent *AgentServer,

	err error,
) {
	agent = &AgentServer{}

	agent.VSockPath = fmt.Sprintf("%s_%d", vsockPath, vsockPort)

	lis, err := net.Listen("unix", agent.VSockPath)
	if err != nil {
		return nil, err
	}

	var closeLock sync.Mutex
	closed := false

	agent.Close = func() {
		closeLock.Lock()
		defer closeLock.Unlock()

		// We need to remove this file first so that the client can't try to reconnect
		_ = os.Remove(agent.VSockPath) // We ignore errors here since the file might already have been removed, but we don't want to use `RemoveAll` cause it could remove a directory

		_ = lis.Close() // We ignore errors here since we might interrupt a network connection

		closed = true
	}

	agent.Accept = func(acceptCtx context.Context, remoteCtx context.Context) (acceptingAgent *AcceptingAgentServer, errs error) {
		acceptingAgent = &AcceptingAgentServer{}

		internalCtx, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
			acceptCtx,
			&errs,
			utils.GetPanicHandlerHooks{},
		)
		defer wait()
		defer cancel()
		defer handlePanics(false)()

		// We use the background context here instead of the internal context because we want to distinguish
		// between a context cancellation from the outside and getting a response
		readyCtx, cancelReadyCtx := context.WithCancel(acceptCtx)
		defer cancelReadyCtx()

		// We intentionally don't call `wg.Add` and `wg.Done` here - we are ok with leaking this
		// goroutine since we return a `Wait()` function
		go func() {
			select {
			// Failure case; we cancelled the internal context before we got a connection
			case <-internalCtx.Done():
				agent.Close() // We ignore errors here since we might interrupt a network connection

			// Happy case; we've got a connection and we want to wait with closing the agent's connections until the context, not the internal context is cancelled
			// We don't do anything here because the `acceptingAgent` context handler must close in order
			case <-readyCtx.Done():
				break
			}
		}()

		conn, err := lis.Accept()
		if err != nil {
			closeLock.Lock()
			defer closeLock.Unlock()

			if closed && errors.Is(err, net.ErrClosed) { // Don't treat closed errors as errors if we closed the connection
				panic(internalCtx.Err())
			}

			panic(errors.Join(ErrAgentClientAcceptFailed, err))
		}

		acceptingAgent.Close = func() error {
			closeLock.Lock()

			_ = conn.Close() // We ignore errors here since we might interrupt a network connection

			closed = true

			closeLock.Unlock()

			if acceptingAgent.Wait != nil {
				return acceptingAgent.Wait()
			}

			return nil
		}

		// We intentionally don't call `wg.Add` and `wg.Done` here - we are ok with leaking this
		// goroutine since we return a `Wait()` function.
		// We still need to `defer handleGoroutinePanic()()` however so that
		// if we cancel the context during this call, we still handle it appropriately
		handleGoroutinePanics(false, func() {
			select {
			// Failure case; we cancelled the internal context before we got a connection
			case <-internalCtx.Done():
				if err := acceptingAgent.Close(); err != nil {
					panic(err)
				}
				agent.Close() // We ignore errors here since we might interrupt a network connection

			// Happy case; we've got a connection and we want to wait with closing the agent's connections until the context, not the internal context is cancelled
			case <-readyCtx.Done():
				<-remoteCtx.Done()

				if err := acceptingAgent.Close(); err != nil {
					panic(err)
				}
				agent.Close() // We ignore errors here since we might interrupt a network connection

				break
			}
		})

		registry := rpc.NewRegistry[remotes.AgentRemote, json.RawMessage](
			&struct{}{},

			remoteCtx, // This resource outlives the current scope, so we use the external context

			&rpc.Options{
				OnClientConnect: func(remoteID string) {
					cancelReadyCtx()
				},
			},
		)

		acceptingAgent.Wait = sync.OnceValue(func() error {
			encoder := json.NewEncoder(conn)
			decoder := json.NewDecoder(conn)

			if err := registry.LinkStream(
				func(v rpc.Message[json.RawMessage]) error {
					return encoder.Encode(v)
				},
				func(v *rpc.Message[json.RawMessage]) error {
					return decoder.Decode(v)
				},

				func(v any) (json.RawMessage, error) {
					b, err := json.Marshal(v)
					if err != nil {
						return nil, err
					}

					return json.RawMessage(b), nil
				},
				func(data json.RawMessage, v any) error {
					return json.Unmarshal([]byte(data), v)
				},
			); err != nil {
				closeLock.Lock()
				defer closeLock.Unlock()

				if closed && errors.Is(err, net.ErrClosed) { // Don't treat closed errors as errors if we closed the connection
					return remoteCtx.Err()
				}

				return errors.Join(ErrAgentClientDisconnected, err)
			}

			return nil
		})

		// We intentionally don't call `wg.Add` and `wg.Done` here - we are ok with leaking this
		// goroutine since we return the process, which allows tracking errors and stopping this goroutine
		// and waiting for it to be stopped. We still need to `defer handleGoroutinePanic()()` however so that
		// any errors we get as we're polling the socket path directory are caught
		// It's important that we start this _after_ calling `cmd.Start`, otherwise our process would be nil
		handleGoroutinePanics(false, func() {
			if err := acceptingAgent.Wait(); err != nil {
				panic(err)
			}
		})

		select {
		case <-internalCtx.Done():
			panic(internalCtx.Err())
		case <-readyCtx.Done():
			break
		}

		found := false
		if err := registry.ForRemotes(func(remoteID string, r remotes.AgentRemote) error {
			acceptingAgent.Remote = r
			found = true

			return nil
		}); err != nil {
			panic(err)
		}

		if !found {
			panic(ErrNoRemoteFound)
		}

		return
	}

	return
}
