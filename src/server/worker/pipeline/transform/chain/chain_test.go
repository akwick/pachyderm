package chain

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/require"
	"github.com/pachyderm/pachyderm/src/server/worker/common"
	"github.com/pachyderm/pachyderm/src/server/worker/datum"
)

type testHasher struct{}

func (th *testHasher) Hash(inputs []*common.Input) string {
	return common.HashDatum("", "", inputs)
}

func makeIndex() map[string]string {
	hasher := &testHasher{}
	result := make(map[string]string)
	names := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r", "s", "t", "u", "v", "w", "x", "y", "z"}
	for _, name := range names {
		hash := hasher.Hash(datumToInputs(name))
		result[hash] = name
	}
	return result
}

var datumIndex = makeIndex()

func printState(jc *jobChain) {
	jc.mutex.Lock()
	defer jc.mutex.Unlock()

	for i, jdi := range jc.jobs {
		flags := ""
		if jdi.additiveOnly {
			flags += " additive"
		}
		if jdi.finished {
			if jdi.allDatums != nil {
				flags += " succeeded"
			} else {
				flags += " failed"
			}
		}
		fmt.Printf("Job %d:%s\n", i, flags)

		ancestors := []int{}
		for _, ancestor := range jdi.ancestors {
			index, err := jc.indexOf(ancestor.data)
			if err != nil {
				index = -1
			}
			ancestors = append(ancestors, index)
		}
		fmt.Printf("ancestors: %v\n", ancestors)

		printDatumSet(jc, "allDatums", jdi.allDatums)
		printDatumSet(jc, "yielding", jdi.yielding)
		printDatumSet(jc, "yielded", jdi.yielded)
	}
}

func printDatumSet(jc *jobChain, name string, set DatumSet) {
	arr := []string{}
	for hash := range set {
		name := datumIndex[hash]
		if name == "" {
			name = "unknown"
		}
		arr = append(arr, name)
	}
	sort.Strings(arr)
	fmt.Printf(" %s (%d): %v\n", name, len(set), arr)
}

type testIterator struct {
	index  int
	inputs [][]*common.Input
}

func (ti *testIterator) Reset() {
	ti.index = -1
}

func (ti *testIterator) Len() int {
	return len(ti.inputs)
}

func (ti *testIterator) Next() bool {
	if ti.index < len(ti.inputs)-1 {
		ti.index++
	}

	return ti.index < len(ti.inputs)
}

func (ti *testIterator) Datum() []*common.Input {
	return ti.inputs[ti.index]
}

func (ti *testIterator) DatumN(n int) []*common.Input {
	return ti.inputs[n]
}

// Convert a test-friendly string to a real fake inputs array
func datumToInputs(name string) []*common.Input {
	return []*common.Input{
		&common.Input{
			Name: "inputRepo",
			FileInfo: &pfs.FileInfo{
				File: &pfs.File{Path: name},
				Hash: []byte(name),
			},
		},
	}
}

func inputsToDatum(inputs []*common.Input) (string, error) {
	if len(inputs) != 1 {
		return "", fmt.Errorf("should only have 1 input for test datums")
	}
	return inputs[0].FileInfo.File.Path, nil
}

func newTestChain(t *testing.T, datums []string) JobChain {
	hasher := &testHasher{}
	chain, err := NewJobChain(hasher)
	require.NoError(t, err)
	require.False(t, chain.Initialized())

	baseDatums := datumsToSet(datums)

	require.NoError(t, chain.Initialize(baseDatums))
	require.True(t, chain.Initialized())

	return chain
}

func datumsToInputs(datums []string) [][]*common.Input {
	inputs := [][]*common.Input{}
	for _, datum := range datums {
		inputs = append(inputs, datumToInputs(datum))
	}
	return inputs
}

func datumsToSet(datums []string) DatumSet {
	hasher := &testHasher{}
	result := make(DatumSet)
	for _, datum := range datums {
		result[hasher.Hash(datumToInputs(datum))] = struct{}{}
	}
	return result
}

func newTestIterator(datums []string) datum.Iterator {
	return &testIterator{inputs: datumsToInputs(datums)}
}

type testJob struct {
	dit datum.Iterator
}

func newTestJob(datums []string) JobData {
	return &testJob{dit: newTestIterator(datums)}
}

func (tj *testJob) Iterator() (datum.Iterator, error) {
	return tj.dit, nil
}

func requireIteratorContents(t *testing.T, jdi JobDatumIterator, expected []string) {
	found := []string{}
	for range expected {
		hasNext, err := jdi.Next(context.Background())
		require.NoError(t, err)
		require.True(t, hasNext)
		datum, err := inputsToDatum(jdi.Datum())
		require.NoError(t, err)
		found = append(found, datum)
	}
	require.ElementsEqual(t, expected, found)
	requireIteratorDone(t, jdi)
}

func requireIteratorDone(t *testing.T, jdi JobDatumIterator) {
	hasNext, err := jdi.Next(context.Background())
	require.NoError(t, err)
	require.False(t, hasNext)
}

func requireIteratorContentsNonBlocking(t *testing.T, jdi JobDatumIterator, expected []string) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	found := []string{}

	require.Equal(t, len(expected), jdi.NumAvailable())
	for range expected {
		hasNext, err := jdi.Next(ctx)
		require.NoError(t, err)
		require.True(t, hasNext)
		datum, err := inputsToDatum(jdi.Datum())
		require.NoError(t, err)
		found = append(found, datum)
	}

	require.ElementsEqual(t, expected, found)
}

func TestEmptyBase(t *testing.T) {
	jobDatums := []string{"a", "b"}
	chain := newTestChain(t, []string{})
	job := newTestJob(jobDatums)
	jdi, err := chain.Start(job)
	require.NoError(t, err)
	requireIteratorContents(t, jdi, jobDatums)
}

func TestAdditiveOnBase(t *testing.T) {
	chain := newTestChain(t, []string{"a"})
	job := newTestJob([]string{"a", "b", "c"})
	jdi, err := chain.Start(job)
	require.NoError(t, err)
	requireIteratorContents(t, jdi, []string{"b", "c"})
}

func TestSubtractiveOnBase(t *testing.T) {
	jobDatums := []string{"a", "c"}
	chain := newTestChain(t, []string{"a", "b", "c"})
	job := newTestJob(jobDatums)
	jdi, err := chain.Start(job)
	require.NoError(t, err)
	requireIteratorContents(t, jdi, jobDatums)
}

func TestAdditiveSubtractiveOnBase(t *testing.T) {
	jobDatums := []string{"b", "c", "d", "e"}
	chain := newTestChain(t, []string{"a", "b", "c"})
	job := newTestJob(jobDatums)
	jdi, err := chain.Start(job)
	require.NoError(t, err)
	requireIteratorContents(t, jdi, jobDatums)
}

// Read from a channel until we have the expected datums, then verify they
// are correct, then make sure the channel doesn't have anything else.
func requireDatums(t *testing.T, datumChan <-chan string, expected []string) {
	// Recvs should be near-instant, but set a decently long timeout to avoid flakiness
	actual := []string{}
loop:
	for range expected {
		select {
		case x, ok := <-datumChan:
			if !ok {
				require.ElementsEqual(t, expected, actual)
			}
			actual = append(actual, x)
		case <-time.After(time.Second):
			break loop
		}
	}
	require.ElementsEqual(t, expected, actual)

	select {
	case x, ok := <-datumChan:
		require.False(t, ok, "datum channel contains extra datum: %s", x)
	default:
	}
}

func requireChannelClosed(t *testing.T, c <-chan string) {
	select {
	case x, ok := <-c:
		require.False(t, ok, "datum channel should be closed, but found extra datum: %s", x)
	case <-time.After(time.Second):
		require.True(t, false, "datum channel should be closed, but it is blocked")
	}
}

func requireChannelBlocked(t *testing.T, c <-chan string) {
	select {
	case x, ok := <-c:
		require.True(t, ok, "datum channel should be blocked, but it is closed")
		require.True(t, false, "datum channel should be blocked, but it contains datum: %s", x)
	default:
	}
}

func superviseTestJobWithError(
	ctx context.Context,
	eg *errgroup.Group,
	jdi JobDatumIterator,
	expectedErr string,
) <-chan string {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	datumsChan := make(chan string)
	eg.Go(func() (retErr error) {
		defer func() {
			if retErr != nil && expectedErr != "" && strings.Contains(retErr.Error(), expectedErr) {
				retErr = nil
			}
		}()

		defer close(datumsChan)
		for {
			ok, err := jdi.Next(ctx)
			if err != nil {
				return err
			}
			if !ok {
				break
			}

			for {
				datum, err := inputsToDatum(jdi.Datum())
				if err != nil {
					return err
				}

				datumsChan <- datum

				if jdi.NumAvailable() == 0 {
					break
				}
				// Because we have more available, this should never block, so use the
				// canceledCtx to make sure
				ok, err := jdi.Next(canceledCtx)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("iterator should have had more available")
				}
			}
		}
		return nil
	})

	return datumsChan
}

func superviseTestJob(ctx context.Context, eg *errgroup.Group, jdi JobDatumIterator) <-chan string {
	return superviseTestJobWithError(ctx, eg, jdi, "")
}

// Job 1: ABCD   -> 1. Succeed
// Job 2:   CDEF  -> 2. Succeed
// Job 3: AB DE GH -> 3. Succeed
func TestSuccess(t *testing.T) {
	chain := newTestChain(t, []string{})
	job1 := newTestJob([]string{"a", "b", "c", "d"})
	job2 := newTestJob([]string{"c", "d", "e", "f"})
	job3 := newTestJob([]string{"a", "b", "d", "e", "g", "h"})

	eg, ctx := errgroup.WithContext(context.Background())

	jdi1, err := chain.Start(job1)
	require.NoError(t, err)
	datums1 := superviseTestJob(ctx, eg, jdi1)

	jdi2, err := chain.Start(job2)
	require.NoError(t, err)
	datums2 := superviseTestJob(ctx, eg, jdi2)

	jdi3, err := chain.Start(job3)
	require.NoError(t, err)
	datums3 := superviseTestJob(ctx, eg, jdi3)

	requireDatums(t, datums1, []string{"a", "b", "c", "d"})
	requireDatums(t, datums2, []string{"e", "f"})
	requireDatums(t, datums3, []string{"g", "h"})
	requireChannelClosed(t, datums1)
	requireChannelBlocked(t, datums2)
	requireChannelBlocked(t, datums3)

	require.NoError(t, chain.Succeed(job1, make(DatumSet)))
	requireDatums(t, datums2, []string{"c", "d"})
	requireDatums(t, datums3, []string{"a", "b"})
	requireChannelClosed(t, datums2)

	require.NoError(t, chain.Succeed(job2, make(DatumSet)))
	requireDatums(t, datums3, []string{"d", "e"})
	requireChannelClosed(t, datums3)

	require.NoError(t, chain.Succeed(job3, make(DatumSet)))
	require.NoError(t, eg.Wait())
}

// Job 1: ABCD   -> 1. Fail
// Job 2:   CDEF  -> 2. Fail
// Job 3: AB DE GH -> 3. Succeed
func TestFail(t *testing.T) {
	chain := newTestChain(t, []string{})
	job1 := newTestJob([]string{"a", "b", "c", "d"})
	job2 := newTestJob([]string{"c", "d", "e", "f"})
	job3 := newTestJob([]string{"a", "b", "d", "e", "g", "h"})

	eg, ctx := errgroup.WithContext(context.Background())

	jdi1, err := chain.Start(job1)
	require.NoError(t, err)
	datums1 := superviseTestJob(ctx, eg, jdi1)

	jdi2, err := chain.Start(job2)
	require.NoError(t, err)
	datums2 := superviseTestJob(ctx, eg, jdi2)

	jdi3, err := chain.Start(job3)
	require.NoError(t, err)
	datums3 := superviseTestJob(ctx, eg, jdi3)

	requireDatums(t, datums1, []string{"a", "b", "c", "d"})
	requireDatums(t, datums2, []string{"e", "f"})
	requireDatums(t, datums3, []string{"g", "h"})
	requireChannelClosed(t, datums1)
	requireChannelBlocked(t, datums2)
	requireChannelBlocked(t, datums3)

	require.NoError(t, chain.Fail(job1))
	requireDatums(t, datums2, []string{"c", "d"})
	requireDatums(t, datums3, []string{"a", "b"})
	requireChannelClosed(t, datums2)

	require.NoError(t, chain.Fail(job2))
	requireDatums(t, datums3, []string{"d", "e"})
	requireChannelClosed(t, datums3)

	require.NoError(t, chain.Succeed(job3, make(DatumSet)))
	require.NoError(t, eg.Wait())
}

// Job 1: AB   -> 1. Succeed
// Job 2: ABC  -> 2. Succeed
func TestAdditiveSuccess(t *testing.T) {
	chain := newTestChain(t, []string{})
	job1 := newTestJob([]string{"a", "b"})
	job2 := newTestJob([]string{"a", "b", "c"})

	eg, ctx := errgroup.WithContext(context.Background())

	jdi1, err := chain.Start(job1)
	require.NoError(t, err)
	datums1 := superviseTestJob(ctx, eg, jdi1)

	jdi2, err := chain.Start(job2)
	require.NoError(t, err)
	datums2 := superviseTestJob(ctx, eg, jdi2)

	requireDatums(t, datums1, []string{"a", "b"})
	requireDatums(t, datums2, []string{"c"})
	requireChannelClosed(t, datums1)
	requireChannelBlocked(t, datums2)

	require.NoError(t, chain.Succeed(job1, make(DatumSet)))
	requireChannelClosed(t, datums2)

	require.NoError(t, chain.Succeed(job2, make(DatumSet)))
	require.NoError(t, eg.Wait())
}

// Job 1: AB   -> 1. Fail
// Job 2: ABC  -> 2. Succeed
func TestAdditiveFail(t *testing.T) {
	chain := newTestChain(t, []string{})
	job1 := newTestJob([]string{"a", "b"})
	job2 := newTestJob([]string{"a", "b", "c"})

	eg, ctx := errgroup.WithContext(context.Background())

	jdi1, err := chain.Start(job1)
	require.NoError(t, err)
	datums1 := superviseTestJob(ctx, eg, jdi1)

	jdi2, err := chain.Start(job2)
	require.NoError(t, err)
	datums2 := superviseTestJob(ctx, eg, jdi2)

	requireDatums(t, datums1, []string{"a", "b"})
	requireDatums(t, datums2, []string{"c"})
	requireChannelClosed(t, datums1)
	requireChannelBlocked(t, datums2)

	require.NoError(t, chain.Fail(job1))
	requireDatums(t, datums2, []string{"a", "b"})
	requireChannelClosed(t, datums2)

	require.NoError(t, chain.Succeed(job2, make(DatumSet)))
	require.NoError(t, eg.Wait())
}

// Job 1: AB   -> 1. Succeed
// Job 2:  BC  -> 2. Succeed
// Job 3:  BCD -> 3. Succeed
func TestCascadeSuccess(t *testing.T) {
	chain := newTestChain(t, []string{})
	job1 := newTestJob([]string{"a", "b"})
	job2 := newTestJob([]string{"b", "c"})
	job3 := newTestJob([]string{"b", "c", "d"})

	eg, ctx := errgroup.WithContext(context.Background())

	jdi1, err := chain.Start(job1)
	require.NoError(t, err)
	datums1 := superviseTestJob(ctx, eg, jdi1)

	jdi2, err := chain.Start(job2)
	require.NoError(t, err)
	datums2 := superviseTestJob(ctx, eg, jdi2)

	jdi3, err := chain.Start(job3)
	require.NoError(t, err)
	datums3 := superviseTestJob(ctx, eg, jdi3)

	requireDatums(t, datums1, []string{"a", "b"})
	requireDatums(t, datums2, []string{"c"})
	requireDatums(t, datums3, []string{"d"})
	requireChannelClosed(t, datums1)
	requireChannelBlocked(t, datums2)
	requireChannelBlocked(t, datums3)

	require.NoError(t, chain.Succeed(job1, make(DatumSet)))
	requireDatums(t, datums2, []string{"b"})
	requireChannelClosed(t, datums2)
	requireChannelBlocked(t, datums3)

	require.NoError(t, chain.Succeed(job2, make(DatumSet)))
	requireChannelClosed(t, datums3)

	require.NoError(t, chain.Succeed(job3, make(DatumSet)))
	require.NoError(t, eg.Wait())
}

// Job 1: AB   -> 1. Succeed
// Job 2: ABC  -> 2. Fail
// Job 3: ABCD -> 3. Succeed
func TestCascadeFail(t *testing.T) {
	chain := newTestChain(t, []string{})
	job1 := newTestJob([]string{"a", "b"})
	job2 := newTestJob([]string{"a", "b", "c"})
	job3 := newTestJob([]string{"a", "b", "c", "d"})

	eg, ctx := errgroup.WithContext(context.Background())

	jdi1, err := chain.Start(job1)
	require.NoError(t, err)
	datums1 := superviseTestJob(ctx, eg, jdi1)

	jdi2, err := chain.Start(job2)
	require.NoError(t, err)
	datums2 := superviseTestJob(ctx, eg, jdi2)

	jdi3, err := chain.Start(job3)
	require.NoError(t, err)
	datums3 := superviseTestJob(ctx, eg, jdi3)

	requireDatums(t, datums1, []string{"a", "b"})
	requireDatums(t, datums2, []string{"c"})
	requireDatums(t, datums3, []string{"d"})
	requireChannelClosed(t, datums1)
	requireChannelBlocked(t, datums2)
	requireChannelBlocked(t, datums3)

	require.NoError(t, chain.Succeed(job1, make(DatumSet)))
	requireChannelClosed(t, datums2)
	requireChannelBlocked(t, datums3)

	require.NoError(t, chain.Fail(job2))
	requireDatums(t, datums3, []string{"c"})
	requireChannelClosed(t, datums3)

	require.NoError(t, chain.Succeed(job3, make(DatumSet)))
	require.NoError(t, eg.Wait())
}

// Job 1: AB   -> 2. Succeed
// Job 2:  BC  -> 1. Fail
// Job 3:  BCD -> 3. Succeed
func TestSplitFail(t *testing.T) {
	chain := newTestChain(t, []string{})
	job1 := newTestJob([]string{"a", "b"})
	job2 := newTestJob([]string{"b", "c"})
	job3 := newTestJob([]string{"b", "c", "d"})

	eg, ctx := errgroup.WithContext(context.Background())

	jdi1, err := chain.Start(job1)
	require.NoError(t, err)
	datums1 := superviseTestJob(ctx, eg, jdi1)

	jdi2, err := chain.Start(job2)
	require.NoError(t, err)
	datums2 := superviseTestJobWithError(ctx, eg, jdi2, "job failed")

	jdi3, err := chain.Start(job3)
	require.NoError(t, err)
	datums3 := superviseTestJob(ctx, eg, jdi3)

	requireDatums(t, datums1, []string{"a", "b"})
	requireDatums(t, datums2, []string{"c"})
	requireDatums(t, datums3, []string{"d"})
	requireChannelClosed(t, datums1)
	requireChannelBlocked(t, datums2)
	requireChannelBlocked(t, datums3)

	require.NoError(t, chain.Fail(job2))
	requireDatums(t, datums3, []string{"c"})
	//requireChannelClosed(t, datums2)
	requireChannelBlocked(t, datums3)

	require.NoError(t, chain.Succeed(job1, make(DatumSet)))
	requireDatums(t, datums3, []string{"b"})
	requireChannelClosed(t, datums3)

	require.NoError(t, chain.Succeed(job3, make(DatumSet)))
	require.NoError(t, eg.Wait())
}

// Job 1: AB   -> 1. Succeed (A and B recovered)
// Job 2: ABC  -> 2. Succeed (A and C recovered)
// Job 3: ABCD -> 3. Succeed
func TestRecoveredDatums(t *testing.T) {
	chain := newTestChain(t, []string{})
	job1 := newTestJob([]string{"a", "b"})
	job2 := newTestJob([]string{"a", "b", "c"})
	job3 := newTestJob([]string{"a", "b", "c", "d"})

	eg, ctx := errgroup.WithContext(context.Background())

	jdi1, err := chain.Start(job1)
	require.NoError(t, err)
	datums1 := superviseTestJob(ctx, eg, jdi1)

	jdi2, err := chain.Start(job2)
	require.NoError(t, err)
	datums2 := superviseTestJob(ctx, eg, jdi2)

	jdi3, err := chain.Start(job3)
	require.NoError(t, err)
	datums3 := superviseTestJob(ctx, eg, jdi3)

	requireDatums(t, datums1, []string{"a", "b"})
	requireDatums(t, datums2, []string{"c"})
	requireDatums(t, datums3, []string{"d"})
	requireChannelClosed(t, datums1)
	requireChannelBlocked(t, datums2)
	requireChannelBlocked(t, datums3)

	require.NoError(t, chain.Succeed(job1, datumsToSet([]string{"a", "b"})))
	requireDatums(t, datums2, []string{"a", "b"})
	requireChannelClosed(t, datums2)
	requireChannelBlocked(t, datums3)

	require.NoError(t, chain.Succeed(job2, datumsToSet([]string{"a", "c"})))
	requireDatums(t, datums3, []string{"a", "c"})
	requireChannelClosed(t, datums3)

	require.NoError(t, chain.Succeed(job3, make(DatumSet)))
	require.NoError(t, eg.Wait())
}

func TestEarlySuccess(t *testing.T) {
	chain := newTestChain(t, []string{})
	job1 := newTestJob([]string{"a", "b"})

	_, err := chain.Start(job1)
	require.NoError(t, err)

	require.YesError(t, chain.Succeed(job1, make(DatumSet)), "items remaining")
}

func TestEarlyFail(t *testing.T) {
	chain := newTestChain(t, []string{})
	job1 := newTestJob([]string{"a", "b"})

	_, err := chain.Start(job1)
	require.NoError(t, err)

	require.NoError(t, chain.Fail(job1))
}