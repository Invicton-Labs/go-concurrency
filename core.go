package concurrency

import (
	"context"
	"reflect"
	"time"

	"github.com/Invicton-Labs/go-stackerr"
	"golang.org/x/sync/errgroup"
)

var (
	DefaultEmptyInputChannelCallbackInterval time.Duration = 1 * time.Second
	DefaultFullOutputChannelCallbackInterval time.Duration = 1 * time.Second
)

type ProcessingFuncWithInputWithOutput[InputType any, OutputType any] func(ctx context.Context, input InputType, metadata *RoutineFunctionMetadata) (output OutputType, err stackerr.Error)
type ProcessingFuncWithInputWithoutOutput[InputType any] func(ctx context.Context, input InputType, metadata *RoutineFunctionMetadata) (err stackerr.Error)
type ProcessingFuncWithoutInputWithOutput[OutputType any] func(ctx context.Context, metadata *RoutineFunctionMetadata) (output OutputType, err stackerr.Error)
type ProcessingFuncWithoutInputWithoutOutput func(ctx context.Context, metadata *RoutineFunctionMetadata) (err stackerr.Error)
type ProcessingFuncTypes[InputType any, OutputType any] interface {
	ProcessingFuncWithInputWithOutput[InputType, OutputType] | ProcessingFuncWithInputWithoutOutput[InputType] | ProcessingFuncWithoutInputWithOutput[OutputType] | ProcessingFuncWithoutInputWithoutOutput
}

// The common set of values for callbacks that are specific to single routine
type RoutineFunctionMetadata struct {
	// The name of the executor
	ExecutorName string
	// The index of the routine (will be in [0-concurrency))
	RoutineIndex uint
	// The index of the input (the input value is the Nth value
	// from the input channel)
	ExecutorInputIndex uint64
	// The index of the input for this routine
	RoutineInputIndex uint64
	// The status tracker for this executor
	RoutineStatusTracker *RoutineStatusTracker
	// The status trackers for all executors in this chain (by map)
	RoutineStatusTrackersMap map[string]*RoutineStatusTracker
	// The status trackers for all executors in this chain (by slice, in order of chaining)
	RoutineStatusTrackersSlice []*RoutineStatusTracker
	// A logger that is sweetened with additional data about the executor/routine
	//Log *zap.SugaredLogger
}

type executorInput[
	InputType any,
	OutputType any,
	OutputChanType any,
	ProcessingFuncType ProcessingFuncTypes[InputType, OutputType],
] struct {
	// REQUIRED. A name that can be used in logs for this executor,
	// and for finding it in callback inputs.
	Name string

	// OPTIONAL. The number of routines to run. Defaults to 1.
	Concurrency int

	// REQUIRED. The function that processes an input into an output.
	Func ProcessingFuncType

	// REQUIRED FOR TOP-LEVEL EXECUTORS (not for chained executors), with the
	// exception of Continuous. The channel that has input values.
	InputChannel <-chan InputType

	// OPTIONAL. The size of the output channel that gets created.
	// Defaults to the twice the Concurrency value. Only applies if
	// this is a non-final executor.
	OutputChannelSize int

	// OPTIONAL. A channel to use for outputs, instead of creating a new
	// one internally.
	OutputChannel chan OutputChanType

	// OPTIONAL. Whether to ignore zero-value outputs from the processing function.
	// If true, zero-value outputs (the default value of the output type) will not
	// be sent downstream.
	IgnoreZeroValueOutputs bool

	// OPTIONAL. Whether to process all remaining upstream executor outputs with the
	// consumer after the upstream executor errors out. Default (false) is to immediately
	// kill the consumer if the upstream executor fails. Has no effect for the
	// top-level executor in a chain.
	ProcessUpstreamOutputsAfterUpstreamError bool

	// OPTIONAL. How long to wait for an input before calling the empty input callback
	// function, IF one has been provided. Defaults to the
	// DefaultEmptyInputChannelCallbackInterval value.
	EmptyInputChannelCallbackInterval time.Duration
	// OPTIONAL. A function to call when the input channel is empty, but not closed.
	// Each routine has its own separate timer, so this could be called many times
	// concurrently by different routines.
	EmptyInputChannelCallback func(input *EmptyInputChannelCallbackInput) stackerr.Error

	// OPTIONAL. How long to wait to write an output before calling the full outputcallback
	// function, IF one has been provided.  Defaults to the
	// DefaultFullOutputChannelCallbackInterval value.
	FullOutputChannelCallbackInterval time.Duration
	// OPTIONAL. A function to call when the output channel is full and an output cannot
	// be written to it. Each routine has its own separate timer, so this could be called many
	// times concurrently by different routines.
	// Only applies if the these options are for a non-final executor.
	FullOutputChannelCallback func(input *FullOutputChannelCallbackInput) stackerr.Error

	// OPTIONAL. A function to call when the routine is about to exit due to an error.
	// Note that this is PER ROUTINE, not when the executor (group of routines) is about to exit.
	RoutineErrorCallback func(input *RoutineErrorCallbackInput) stackerr.Error
	// OPTIONAL. A function to call when the routine is about to exit due to the
	// input channel being closed. Note that this is PER ROUTINE,
	// not when the executor (group of routines) is about to exit.
	RoutineSuccessCallback func(input *RoutineSuccessCallbackInput) stackerr.Error
	// OPTIONAL. A function to call when a routine is about to exit due to the context
	// being done. Note that this is PER ROUTINE, not when the executor (group of routines)
	// is about to exit.
	RoutineContextDoneCallback func(input *RoutineContextDoneCallbackInput) stackerr.Error

	// OPTIONAL. A function to call ONCE when all routines in the executor are finished
	// and there was an error in one or more routines.
	ExecutorErrorCallback func(input *ExecutorErrorCallbackInput) stackerr.Error
	// OPTIONAL. A function to call ONCE when all routines in the executor are finished
	// and there were no errors.
	ExecutorSuccessCallback func(input *ExecutorSuccessCallbackInput) stackerr.Error
	// OPTIONAL. A function to call ONCE when all routines in the executor are finished
	// and the context was cancelled.
	ExecutorContextDoneCallback func(input *ExecutorContextDoneCallbackInput) stackerr.Error

	// OPTIONAL. The number of elements in each batch. Only used for executors that batch outputs.
	BatchSize int

	// OPTIONAL. The maximum amount of time to hold a batch before outputting it, even if it's
	// not full. If 0, it will always wait for a full batch before outputting
	// it. Only used for executors that batch outputs.
	BatchMaxPeriod time.Duration

	// Internal use only. Output from the upstream executor.
	upstream *ExecutorOutput[InputType]
}

type upstreamCtxCancel struct {
	upstream   *upstreamCtxCancel
	cancelFunc context.CancelFunc
}

func (ucc *upstreamCtxCancel) cancel() {
	// Cancel the context for this level
	ucc.cancelFunc()
	// Recurse up to the top
	if ucc.upstream != nil {
		ucc.upstream.cancel()
	}
}

type ExecutorOutput[OutputChanType any] struct {

	// A context that is derived from the top-level executor's
	// input context and is cancelled if any of the executors in
	// a chain fail (after they are all cleaned up).
	ctx context.Context

	// A channel that only gets closed if the executor finishes with
	// an error.
	errChan <-chan struct{}

	// The name of the executor
	Name string

	// The status tracker for the routines.
	RoutineStatusTracker *RoutineStatusTracker

	// The channel that the outputs are written to
	OutputChan <-chan OutputChanType

	// Internal use only. A function to cancel the output (passthrough) context.
	passthroughCtxCancel context.CancelFunc

	// Internal use only. The error group that is used for the executor routines.
	errorGroup *errgroup.Group

	// Internal use only. The internal context used by the executor chain.
	//internalCtx context.Context
	// Internal use only. The cancellation function for the internal context
	// used by the executor chain.
	//internalCtxCancel context.CancelFunc

	// Internal use only. The slice of status trackers.
	routineStatusTrackersSlice []*RoutineStatusTracker
	// Internal use only. The map of status trackers.
	routineStatusTrackersMap map[string]*RoutineStatusTracker

	upstreamCtxCancel *upstreamCtxCancel
}

// Wait waits for an executor to finish. If the executor exited with an error,
// that error will be returned.
func (eo *ExecutorOutput[OutputChanType]) Wait() stackerr.Error {
	err := stackerr.Wrap(eo.errorGroup.Wait())
	// We use a separate context for output/passthrough than we
	// do for the error group, in order to handle cleanup related
	// tasks. However, it is expected that calling "Wait()" will
	// also finish the context, so we must manually cancel it.
	eo.passthroughCtxCancel()
	return err
}

// Ctx returns a context that is derived from the top-level executor's input context and is cancelled
// if any of the executors in a chain fail (after they are all cleaned up).
func (eo *ExecutorOutput[OutputChanType]) Ctx() context.Context {
	return eo.ctx
}

// Errored returns a channel that never has a value, but will always remain open
// UNLESS this executor finished with an error.
func (eo *ExecutorOutput[OutputChanType]) Errored() <-chan struct{} {
	return eo.errChan
}

func new[
	InputType any,
	OutputType any,
	OutputChanType any,
	ProcessingFuncType ProcessingFuncTypes[InputType, OutputType],
](
	ctx context.Context,
	input executorInput[InputType, OutputType, OutputChanType, ProcessingFuncType],
	outputFunc func(
		input *saveOutputSettings[OutputChanType],
		value OutputType,
		executorInputIndex uint64,
		routineInputIndex uint64,
		lastOutput *time.Time,
		callbackTracker *timeTracker,
		forceSendBatch bool,
	) (
		err stackerr.Error,
	),
	batchMaxInterval time.Duration,
	forceWaitForInput bool,
) *ExecutorOutput[OutputChanType] {
	if ctx == nil {
		ctx = context.Background()
	}
	if input.Name == "" {
		panic("input.Name cannot be an empty string")
	}
	if input.Func == nil {
		panic("input.Func cannot be nil")
	}
	if input.upstream != nil && input.InputChannel != nil {
		panic("input.InputChannel cannot be provided for chained executors")
	}
	if outputFunc == nil && input.OutputChannel != nil {
		panic("input.OutputChannel must be nil if outputFunc is nil")
	}
	if input.Concurrency < 0 {
		panic("input.Concurrency must not be less than 0")
	}
	if input.Concurrency == 0 {
		input.Concurrency = 1
	}

	// This is a context that is used internally for the routines in this executor. It
	// gets cancelled as soon as any of the routines in this executor returns an error or,
	// if we don't want to continue processing upstream outputs, any upstream executor
	// encounters an error.

	// By default, wrap the input context. This way, if the input context ever gets cancelled,
	// it will also cancel all routines in this executor.
	internalCtx, internalCtxCancel := context.WithCancel(ctx)
	// If, however, we want to continue to process upstream outputs after an upstream failure
	// (i.e. keep processing anything still remaining in the input channel), then use a version
	// of the context that has access to the same values, but won't be cancelled if the upstream
	// context is cancelled.
	if input.ProcessUpstreamOutputsAfterUpstreamError {
		internalCtx, internalCtxCancel = newExecutorContext(ctx)
	}

	// Create a context that will be passed out. This context only gets
	// cancelled when the final routine in this executor exits out. We use
	// this instead of the error group context because we want to complete
	// our clean-up, callbacks, etc. before downstream users of the context
	// cancel their own activities, which could lead to termination of the
	// program before our cleanup/callbacks are complete.
	passthroughCtx, passthroughCtxCancel := newExecutorContext(ctx)

	var outputChan chan OutputChanType = nil
	// If we haven't disabled the output channel
	if outputFunc != nil {
		// If an external one was provided, use it
		if input.OutputChannel != nil {
			outputChan = input.OutputChannel
		} else {
			// We want outputs but a channel wasn't specified, so
			// create one.
			outputChan = make(chan OutputChanType, zeroDefault(input.OutputChannelSize, 2*input.Concurrency))
		}
	}

	var inputChan <-chan InputType

	var routineStatusTrackersSlice []*RoutineStatusTracker

	// Create a new routine status tracker struct
	routineStatusTracker := &RoutineStatusTracker{
		executorName:       input.Name,
		numRoutinesRunning: int32(input.Concurrency),
		getInputChanLength: func() int {
			return len(input.InputChannel)
		},
		getOutputChanLength: func() *int {
			if outputChan != nil {
				l := len(outputChan)
				return &l
			}
			return nil
		},
	}

	// Check if there's an upstream executor in the chain
	if input.upstream != nil {
		// There's an upstream executor
		inputChan = input.upstream.OutputChan
		// Create a new slice (so appending to it doesn't append to upstream copies)
		routineStatusTrackersSlice = make([]*RoutineStatusTracker, len(input.upstream.routineStatusTrackersSlice), len(input.upstream.routineStatusTrackersSlice)+1)
		// Copy the slice
		copy(routineStatusTrackersSlice, input.upstream.routineStatusTrackersSlice)
		routineStatusTrackersSlice = append(routineStatusTrackersSlice, routineStatusTracker)
	} else {
		// If there isn't an upstream value, then this executor
		// must be the first in the chain.
		inputChan = input.InputChannel
		routineStatusTrackersSlice = []*RoutineStatusTracker{routineStatusTracker}
	}

	// Create an error group for the routines. We don't need to create a
	// context because we manage the context separately.
	errGroup := &errgroup.Group{}

	// Create a map version of the status trackers
	routineStatusTrackersMap := map[string]*RoutineStatusTracker{}
	for _, v := range routineStatusTrackersSlice {
		routineStatusTrackersMap[v.executorName] = v
	}

	// A counter for the number of inputs that have been taken from
	// the input channel.
	var inputIndex uint64 = 0
	var outputIndex uint64 = 0

	baseCallbackInput := &BaseExecutorCallbackInput{
		ExecutorName: input.Name,
	}

	upstreamCancellation := &upstreamCtxCancel{
		cancelFunc: internalCtxCancel,
	}
	if input.upstream != nil {
		upstreamCancellation.upstream = input.upstream.upstreamCtxCancel
	}

	routineExitSettings := &routineExitSettings[InputType, OutputType, OutputChanType, ProcessingFuncType]{
		executorInput:        &input,
		upstreamCtxCancel:    upstreamCancellation,
		passthroughCtxCancel: passthroughCtxCancel,
		// Create a channel that will be closed ONLY if this executor exits with an error.
		errChan:                   make(chan struct{}),
		routineStatusTracker:      routineStatusTracker,
		outputChan:                outputChan,
		baseExecutorCallbackInput: baseCallbackInput,
	}

	var isBatchOutput bool
	if outputChan != nil {
		var outputZero OutputType
		outputType := reflect.TypeOf(outputZero)
		var outputChanZero OutputChanType
		outputChanType := reflect.TypeOf(outputChanZero)
		isBatchOutput = outputChanType.Kind() == reflect.Slice && outputChanType.Elem() == outputType
	}

	batchTimeTracker := newTimeTracker(input.BatchMaxPeriod, true)

	routineSettings := &routineSettings[InputType, OutputType, OutputChanType, ProcessingFuncType]{
		executorInput:                     &input,
		internalCtx:                       internalCtx,
		upstreamCtxCancel:                 upstreamCancellation,
		passthroughCtxCancel:              passthroughCtxCancel,
		routineStatusTracker:              routineStatusTracker,
		routineStatusTrackersSlice:        routineStatusTrackersSlice,
		routineStatusTrackersMap:          routineStatusTrackersMap,
		inputIndexCounter:                 &inputIndex,
		outputIndexCounter:                &outputIndex,
		emptyInputChannelCallbackInterval: zeroDefault(input.EmptyInputChannelCallbackInterval, DefaultEmptyInputChannelCallbackInterval),
		fullOutputChannelCallbackInterval: zeroDefault(input.FullOutputChannelCallbackInterval, DefaultFullOutputChannelCallbackInterval),
		inputChan:                         inputChan,
		outputChan:                        outputChan,
		outputFunc:                        outputFunc,
		batchTimeTracker:                  batchTimeTracker,
		isBatchOutput:                     isBatchOutput,
		forceWaitForInput:                 forceWaitForInput,
		exitFunc: getRoutineExit(
			routineExitSettings,
		),
	}

	// This handles the two different types of processing functions we might get
	switch any(input.Func).(type) {
	case ProcessingFuncWithInputWithOutput[InputType, OutputType]:
		routineSettings.processingFuncWithInputWithOutput = any(input.Func).(ProcessingFuncWithInputWithOutput[InputType, OutputType])
		if inputChan == nil {
			panic("Cannot have a processing func with an input, but no input channel to pull inputs from")
		}
		if outputChan == nil {
			panic("Cannot have a processing func with an output, but no output channel to output to")
		}
	case ProcessingFuncWithInputWithoutOutput[InputType]:
		if inputChan == nil {
			panic("Cannot have a processing func with an input, but no input channel to pull inputs from")
		}
		if outputChan != nil {
			panic("Cannot have an output channel when the processing func does not return an output")
		}
		routineSettings.processingFuncWithInputWithoutOutput = any(input.Func).(ProcessingFuncWithInputWithoutOutput[InputType])
	case ProcessingFuncWithoutInputWithOutput[OutputType]:
		if inputChan != nil && !forceWaitForInput {
			panic("Cannot have an input channel when the processing func does not return an input")
		}
		if outputChan == nil {
			panic("Cannot have a processing func with an output, but no output channel to output to")
		}
		routineSettings.processingFuncWithoutInputWithOutput = any(input.Func).(ProcessingFuncWithoutInputWithOutput[OutputType])
	case ProcessingFuncWithoutInputWithoutOutput:
		if inputChan != nil && !forceWaitForInput {
			panic("Cannot have an input channel when the processing func does not return an input")
		}
		if outputChan != nil {
			panic("Cannot have an output channel when the processing func does not return an output")
		}
		routineSettings.processingFuncWithoutInputWithoutOutput = any(input.Func).(ProcessingFuncWithoutInputWithoutOutput)
	default:
		panic("Unrecognized processing function signature")
	}

	// Start the same number of routines as the concurrency
	for i := 0; i < input.Concurrency; i++ {
		errGroup.Go(getRoutine(
			routineSettings,
			uint(i),
		))
	}

	return &ExecutorOutput[OutputChanType]{
		ctx:                        passthroughCtx,
		errChan:                    routineExitSettings.errChan,
		Name:                       input.Name,
		RoutineStatusTracker:       routineStatusTracker,
		OutputChan:                 outputChan,
		routineStatusTrackersSlice: routineStatusTrackersSlice,
		routineStatusTrackersMap:   routineStatusTrackersMap,
		errorGroup:                 errGroup,
		passthroughCtxCancel:       passthroughCtxCancel,
		upstreamCtxCancel:          upstreamCancellation,
	}
}
