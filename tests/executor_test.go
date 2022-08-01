package tests

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/invicton-labs/concurrency"
)

func TestExecutor(t *testing.T) {
	testMultiConcurrencies(t, "executor", executor)
}

func executor(t *testing.T, numRoutines int, inputCount int) {
	ctx := context.Background()
	inputChan := make(chan int, inputCount)
	for i := 1; i <= inputCount; i++ {
		inputChan <- i
	}
	var received int32 = 0
	executor := concurrency.Executor(ctx, concurrency.ExecutorInput[int, uint]{
		Name:              "test-executor-1",
		Concurrency:       numRoutines,
		OutputChannelSize: inputCount * 2,
		InputChannel:      inputChan,
		Func: func(ctx context.Context, input int, metadata *concurrency.RoutineFunctionMetadata) (output uint, err error) {
			atomic.AddInt32(&received, 1)
			return uint(input), nil
		},
		EmptyInputChannelCallback: emptyInput,
		FullOutputChannelCallback: fullOutput,
	})
	time.Sleep(100 * time.Millisecond)
	for i := 1; i <= inputCount; i++ {
		inputChan <- i
	}
	close(inputChan)
	if err := executor.Wait(); err != nil {
		t.Error(err)
		return
	}
	if int(received) != 2*inputCount {
		t.Errorf("Received %d inputs, but expected %d\n", received, 2*inputCount)
		return
	}
	maxFound := uint(0)
	numOutput := 0
	for {
		v, open := <-executor.OutputChan
		if !open {
			break
		}
		if v > maxFound {
			maxFound = v
		}
		numOutput++
	}
	if maxFound != uint(inputCount) {
		t.Errorf("Expected max return value of %d, but received %d\n", inputCount, maxFound)
		return
	}
	if numOutput != 2*inputCount {
		t.Errorf("Received %d outputs, but expected %d\n", numOutput, 2*inputCount)
		return
	}
	verifyCleanup(t, executor)
}

func TestExecutorBatchNoTimeout(t *testing.T) {
	testMultiConcurrencies(t, "executor-batch-no-timeout", executorBatchNoTimeout)
}

func executorBatchNoTimeout(t *testing.T, numRoutines int, inputCount int) {
	ctx := context.Background()
	inputChan := make(chan int, inputCount)
	for i := 1; i <= inputCount; i++ {
		inputChan <- i
	}
	close(inputChan)
	batchSize := 100
	executor := concurrency.ExecutorBatch(ctx, concurrency.ExecutorBatchInput[int, uint]{
		Name:              "test-executor-batch-no-timeout-1",
		Concurrency:       numRoutines,
		OutputChannelSize: inputCount * 2,
		InputChannel:      inputChan,
		Func: func(ctx context.Context, input int, metadata *concurrency.RoutineFunctionMetadata) (uint, error) {
			return uint(input), nil
		},
		BatchSize:                 batchSize,
		EmptyInputChannelCallback: emptyInput,
		FullOutputChannelCallback: fullOutput,
	})
	if err := executor.Wait(); err != nil {
		t.Error(err)
		return
	}
	batchCount := 0
	expectedBatchCount := int(math.Ceil(float64(inputCount)/float64(batchSize)) + 0.1)
	finalBatchSize := inputCount % batchSize
	if finalBatchSize == 0 {
		finalBatchSize = batchSize
	}
	for {
		select {
		case v, open := <-executor.OutputChan:
			if !open {
				goto done
			}
			batchCount++
			if batchCount < expectedBatchCount && len(v) != batchSize {
				t.Errorf("Expected non-final output batch to be full with %d values, but it had %d values", batchSize, len(v))
				return
			}
			if batchCount == expectedBatchCount && len(v) != finalBatchSize {
				t.Errorf("Expected final output batch to have %d values, but it had %d values", finalBatchSize, len(v))
				return
			}
			if batchCount > expectedBatchCount {
				t.Errorf("Received more batches than expected (%d)", expectedBatchCount)
				return
			}
		}
	}
done:
	if batchCount != expectedBatchCount {
		t.Errorf("Received fewer batches (%d) than expected (%d)", batchCount, expectedBatchCount)
		return
	}
	verifyCleanup(t, executor)
}

func TestExecutorBatchTimeout(t *testing.T) {
	testMultiConcurrencies(t, "executor-batch-no-timeout", executorBatchTimeout)
}

func executorBatchTimeout(t *testing.T, numRoutines int, inputCount int) {
	ctx, ctxCancel := context.WithCancel(context.Background())
	inputChan := make(chan int, inputCount)
	for i := 1; i <= inputCount; i++ {
		inputChan <- i
	}
	close(inputChan)
	batchMaxInterval := 100 * time.Millisecond
	batchSize := 100
	sleepPoint := 250
	if inputCount < sleepPoint {
		sleepPoint = inputCount
	}
	executor := concurrency.ExecutorBatch(ctx, concurrency.ExecutorBatchInput[int, uint]{
		Name:                           "test-executor-batch-timeout-1",
		Concurrency:                    numRoutines,
		OutputChannelSize:              inputCount * 2,
		InputChannel:                   inputChan,
		IncludeMetadataInFunctionCalls: true,
		Func: func(ctx context.Context, input int, metadata *concurrency.RoutineFunctionMetadata) (uint, error) {
			if metadata.InputIndex >= uint64(sleepPoint) {
				select {
				case <-ctx.Done():
				case <-time.After(100 * time.Hour):
				}
			}
			return uint(input), nil
		},
		BatchSize:                 batchSize,
		EmptyInputChannelCallback: emptyInput,
		FullOutputChannelCallback: fullOutput,
		BatchMaxInterval:          &batchMaxInterval,
	})
	expectedBatchCount := int(math.Ceil(float64(sleepPoint)/float64(batchSize)) + 0.1)
	finalBatchSize := sleepPoint % batchSize
	if finalBatchSize == 0 {
		finalBatchSize = batchSize
	}
	for i := 0; i < expectedBatchCount; i++ {
		v, open := <-executor.OutputChan
		if !open {
			t.Fatalf("Channel unexpectedly closed")
		}
		if i < expectedBatchCount-1 && len(v) != batchSize {
			t.Fatalf("Expected non-final output batch to be full with %d values, but it had %d values", batchSize, len(v))
		}
		if i == expectedBatchCount-1 && len(v) != finalBatchSize {
			t.Fatalf("Expected final output batch to have %d values, but it had %d values", finalBatchSize, len(v))
		}
	}
	ctxCancel()
	if err := executor.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Expected a context cancelled error, but got %v", err)
	}
	verifyCleanup(t, executor)
}

func TestExecutorFinal(t *testing.T) {
	testMultiConcurrencies(t, "executor-final", executorFinal)
}
func executorFinal(t *testing.T, numRoutines int, inputCount int) {
	ctx := context.Background()
	inputChan := make(chan int, inputCount)
	for i := 1; i <= inputCount; i++ {
		inputChan <- i
	}
	close(inputChan)
	var received int32 = 0
	executor := concurrency.ExecutorFinal(ctx, concurrency.ExecutorFinalInput[int]{
		Name:              "test-executor-final-1",
		Concurrency:       numRoutines,
		OutputChannelSize: inputCount * 2,
		InputChannel:      inputChan,
		Func: func(ctx context.Context, input int, metadata *concurrency.RoutineFunctionMetadata) (err error) {
			atomic.AddInt32(&received, 1)
			return nil
		},
		EmptyInputChannelCallback: emptyInput,
		FullOutputChannelCallback: fullOutput,
	})

	if err := executor.Wait(); err != nil {
		t.Error(err)
		return
	}
	if int(received) != inputCount {
		t.Errorf("Received %d inputs, but expected %d\n", received, inputCount)
		return
	}
	verifyCleanup(t, executor)
}

func TestExecutorError(t *testing.T) {
	testMultiConcurrencies(t, "executor-error", executorError)
}
func executorError(t *testing.T, numRoutines int, inputCount int) {
	ctx := context.Background()
	inputChan := make(chan int, inputCount)
	for i := 1; i <= inputCount; i++ {
		inputChan <- i
	}
	close(inputChan)
	executor := concurrency.Executor(ctx, concurrency.ExecutorInput[int, uint]{
		Name:              "test-executor-error-1",
		Concurrency:       numRoutines,
		OutputChannelSize: inputCount * 2,
		InputChannel:      inputChan,
		Func: func(ctx context.Context, input int, metadata *concurrency.RoutineFunctionMetadata) (output uint, err error) {
			if input > inputCount/2 {
				return 0, fmt.Errorf("test-error")
			}
			return uint(input), nil
		},
		EmptyInputChannelCallback: emptyInput,
		FullOutputChannelCallback: fullOutput,
	})
	err := executor.Wait()
	if err == nil {
		t.Errorf("Expected an error, received none")
		return
	}
	if err.Error() != "test-error" {
		t.Errorf("Received unexpected error string: %s", err.Error())
		return
	}
	verifyCleanup(t, executor)
}
