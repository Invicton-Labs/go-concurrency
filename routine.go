package concurrency

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type routineExitSettings[
	InputType any,
	OutputType any,
	OutputChanType any,
] struct {
	executorInput             *executorInput[InputType, OutputType, OutputChanType]
	upstreamCtxCancel         *upstreamCtxCancel
	passthroughCtxCancel      context.CancelFunc
	routineStatusTracker      *RoutineStatusTracker
	outputChan                chan OutputChanType
	baseExecutorCallbackInput *BaseExecutorCallbackInput
}

func getRoutineExit[
	InputType any,
	OutputType any,
	OutputChanType any,
](
	settings *routineExitSettings[InputType, OutputType, OutputChanType],
) func(err error, routineIdx uint) error {
	var errLock sync.Mutex
	var exitErr error
	return func(err error, routineIdx uint) error {

		// Convert panics into errors
		if r := recover(); r != nil {
			if perr, ok := r.(error); ok {
				err = perr
			} else {
				err = fmt.Errorf("%v", r)
			}
		}

		// Check if this routine threw an error
		if err != nil {
			// If it did, save it as the global exit error for the executor.
			// Even if it's just a context error, it will still trigger
			// the termination of all routines for this executor.
			errLock.Lock()
			if exitErr == nil {
				exitErr = err
			}
			errLock.Unlock()

			// Update the status of this routine
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				settings.routineStatusTracker.updateRoutineStatus(routineIdx, ContextDone)
			} else {
				settings.routineStatusTracker.updateRoutineStatus(routineIdx, Errored)
			}

			// As soon as one routine fails, it's game over for everything in this executor
			// AND every upstream executor, because all upstream results would die here
			// anyways. Cancel the internal context and all upstream contexts.
			settings.upstreamCtxCancel.cancel()

		} else {
			settings.routineStatusTracker.updateRoutineStatus(routineIdx, Finished)
		}

		// If it's the last routine to exit, do some special things
		if settings.routineStatusTracker.GetNumRoutinesRunning() == 0 {

			// Get the original error that triggered the termination of the routines.
			errLock.Lock()
			err = exitErr
			errLock.Unlock()

			// Check if this executor was terminated intentionally
			if err != nil {
				// The executor was terminated intentionally, either by an error in one
				// of the executor's routines or by a cancellation of the context.

				// Check if it was a context cancellation
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					// The context was cancelled.

					// If there are upstream executors, wait for them to finish.
					// Use the upstream exit error as this executor's exit error.
					if settings.executorInput.upstream != nil {
						err = settings.executorInput.upstream.Wait()
					}

					// Otherwise, there are no upstream executors, which means that the context
					// cancellation was done by something external to our executor chain. We have no
					// idea what that might be, so just return the context cancellation error.

					// Run the callback for the executor's cancellation
					if settings.executorInput.ExecutorContextDoneCallback != nil {
						newErr := settings.executorInput.ExecutorContextDoneCallback(&ExecutorContextDoneCallbackInput{
							settings.baseExecutorCallbackInput,
							err,
						})
						if newErr != nil {
							err = newErr
						}
					}

				} else {
					// It wasn't a context cancellation, so it must have been an error in this routine.

					// Wait for all upstream executors to complete, but we don't care about their returned
					// errors because we want to return the error from this executor.
					if settings.executorInput.upstream != nil {
						settings.executorInput.upstream.Wait()
					}

					// Run the callback for the executor's failure
					if settings.executorInput.ExecutorErrorCallback != nil {
						newErr := settings.executorInput.ExecutorErrorCallback(&ExecutorErrorCallbackInput{
							settings.baseExecutorCallbackInput,
							err,
						})
						if newErr != nil {
							err = newErr
						}
					}

					// Now this executor will exit with the error that was generated within this executor.
				}

			} else {
				// None of the routines in this executor threw an error, so all must have completed successfully.

				// However, this does not necessarily mean that all upstream executors completed successfully.
				// They close their channels even if they throw errors, so we need to wait on them and check
				// if they errored out.
				if settings.executorInput.upstream != nil {
					err = settings.executorInput.upstream.Wait()
				}

				if err != nil {
					// This executor completed successfully, but an upstream executor then failed afterwards.
					// We consider this to be a context cancellation, so run the appropriate callback.
					if settings.executorInput.ExecutorContextDoneCallback != nil {
						newErr := settings.executorInput.ExecutorContextDoneCallback(&ExecutorContextDoneCallbackInput{
							settings.baseExecutorCallbackInput,
							err,
						})
						if newErr != nil {
							err = newErr
						}
					}
				} else {
					// This executor completed successfully, and all upstream executors also completed
					// successfully.
					// Run the callback for the executor's successful completion.
					if settings.executorInput.ExecutorSuccessCallback != nil {
						newErr := settings.executorInput.ExecutorSuccessCallback(&ExecutorSuccessCallbackInput{
							settings.baseExecutorCallbackInput,
						})
						if newErr != nil {
							err = newErr
						}
					}
				}
			}

			// When the routines have finished, whether that be due to an error
			// or due to them completing their task (no more inputs to process),
			// close the output channel. This signals to downstream executor that
			// there are no more inputs to process, so they can finish as well.
			// Only do this, though, if we created the output channel in this executor
			// and it wasn't passed in as an option. We don't want to close a channel
			// we didn't create.
			if settings.outputChan != nil && settings.executorInput.OutputChannel == nil {
				close(settings.outputChan)
			}

			if err != nil {
				// If an error occured at all, here or higher in the chain,
				// cancel our passthrough context.
				settings.passthroughCtxCancel()
			}
			return err
		} else {
			// The final routine to exit will return this error instead.
			return nil
		}
	}
}

type routineSettings[
	InputType any,
	OutputType any,
	OutputChanType any,
] struct {
	executorInput                     *executorInput[InputType, OutputType, OutputChanType]
	internalCtx                       context.Context
	upstreamCtxCancel                 *upstreamCtxCancel
	passthroughCtxCancel              context.CancelFunc
	routineStatusTracker              *RoutineStatusTracker
	routineStatusTrackersSlice        []*RoutineStatusTracker
	routineStatusTrackersMap          map[string]*RoutineStatusTracker
	inputIndexCounter                 *uint64
	outputIndexCounter                *uint64
	fullOutputChannelCallbackInterval time.Duration
	emptyInputChannelCallbackInterval time.Duration
	inputChan                         <-chan InputType
	outputChan                        chan OutputChanType
	outputFunc                        func(
		settings *saveOutputSettings[OutputChanType],
		value OutputType,
		inputIndex uint64,
	) (
		err error,
	)
	exitFunc func(err error, routineIdx uint) error
}

func getRoutine[
	InputType any,
	OutputType any,
	OutputChanType any,
](
	settings *routineSettings[InputType, OutputType, OutputChanType],
	routineIdx uint,
) func() error {
	// Mark this new routine as initializing
	settings.routineStatusTracker.updateRoutineStatus(routineIdx, Initializing)

	lastOutput := &lastOutputTimeTracker{}

	routineFunctionMetadata := &RoutineFunctionMetadata{
		ExecutorName:               settings.executorInput.Name,
		RoutineIndex:               routineIdx,
		RoutineStatusTracker:       settings.routineStatusTracker,
		RoutineStatusTrackersMap:   settings.routineStatusTrackersMap,
		RoutineStatusTrackersSlice: settings.routineStatusTrackersSlice,
	}

	// This function gets the routine function metadata. We use an existing struct
	// so we don't need to spend the time/memory creating a new one for each function
	// call. We just update the input index as necessary and use the existing one.
	getRoutineFunctionMetadata := func(inputIdx uint64) *RoutineFunctionMetadata {
		routineFunctionMetadata.InputIndex = inputIdx
		return routineFunctionMetadata
	}

	// The function to call if the context has been cancelled
	ctxCancelledFunc := func(inputIdx uint64) error {
		// If we have a callback for this, call it and return the value
		if settings.executorInput.RoutineContextDoneCallback != nil {
			return settings.executorInput.RoutineContextDoneCallback(&RoutineContextDoneCallbackInput{
				RoutineFunctionMetadata: getRoutineFunctionMetadata(inputIdx),
				Err:                     settings.internalCtx.Err(),
			})
		}
		// Return the error that caused the context to cancel
		return settings.internalCtx.Err()
	}

	getInputSettings := &getInputSettings[InputType, OutputType, OutputChanType]{
		ctxCancelledFunc:                  ctxCancelledFunc,
		internalCtx:                       settings.internalCtx,
		executorInput:                     settings.executorInput,
		emptyInputChannelCallbackInterval: settings.emptyInputChannelCallbackInterval,
		inputChan:                         settings.inputChan,
		getRoutineFunctionMetadata:        getRoutineFunctionMetadata,
	}

	saveOutputSettings := &saveOutputSettings[OutputChanType]{
		ctxCancelledFunc:                  ctxCancelledFunc,
		internalCtx:                       settings.internalCtx,
		fullOutputChannelCallbackInterval: settings.fullOutputChannelCallbackInterval,
		fullOutputChannelCallback:         settings.executorInput.FullOutputChannelCallback,
		ignoreZeroValueOutputs:            settings.executorInput.IgnoreZeroValueOutputs,
		outputChan:                        settings.outputChan,
		outputIndexCounter:                settings.outputIndexCounter,
		getRoutineFunctionMetadata:        getRoutineFunctionMetadata,
		lastOutputTimeTracker:             lastOutput,
	}

	return func() (err error) {

		defer func() {
			err = settings.exitFunc(err, routineIdx)
		}()

		// These track the times of the last successful input pull and
		// output push
		now := time.Now()
		lastInput := now

		var metadata *RoutineFunctionMetadata

		for {
			// Find the index of this input retrieval
			inputIndex := atomic.AddUint64(settings.inputIndexCounter, 1) - 1

			// Get the input from the input channel
			input, inputChanClosed, err := getInput(getInputSettings, inputIndex, &lastInput)
			// If there was an error, or the input channel is closed, exit
			if err != nil || inputChanClosed {
				return err
			}
			// Update the last input timestamp
			lastInput = time.Now()

			if settings.executorInput.IncludeMetadataInFunctionCalls {
				metadata = getRoutineFunctionMetadata(inputIndex)
			}
			output, err := settings.executorInput.Func(settings.internalCtx, input, metadata)
			if err != nil {
				// First check if the context has been cancelled. If it has been, return
				// that error instead of the processing error, since we don't really care
				// about the processing error if the context was cancelled anyways.
				select {
				case <-settings.internalCtx.Done():
					return ctxCancelledFunc(inputIndex)
				default:
					break
				}

				// If there's a callback for the function throwing an error, call it
				if settings.executorInput.RoutineErrorCallback != nil {
					return settings.executorInput.RoutineErrorCallback(&RoutineErrorCallbackInput{
						RoutineFunctionMetadata: getRoutineFunctionMetadata(inputIndex),
						Err:                     err,
					})
				}
				// Otherwise, just return the error
				return err
			}

			// If there's an output function to output with, output the result
			if settings.outputFunc != nil {
				err := settings.outputFunc(saveOutputSettings, output, inputIndex)
				if err != nil {
					return err
				}
			}
		}
	}
}