package ingester

import (
	"context"
	"encoding/binary"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/tempo/modules/overrides"
	"github.com/grafana/tempo/pkg/model"
	"github.com/grafana/tempo/pkg/model/trace"
	"github.com/grafana/tempo/pkg/tempopb"
	v1_trace "github.com/grafana/tempo/pkg/tempopb/trace/v1"
	"github.com/grafana/tempo/pkg/util/test"
)

const testTenantID = "fake"

type ringCountMock struct {
	count int
}

func (m *ringCountMock) HealthyInstancesCount() int {
	return m.count
}

func TestInstance(t *testing.T) {
	request := makeRequest([]byte{})

	i, ingester := defaultInstance(t)

	err := i.PushBytesRequest(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, int(i.traceCount.Load()), len(i.traces))

	err = i.CutCompleteTraces(0, true)
	require.NoError(t, err)
	require.Equal(t, int(i.traceCount.Load()), len(i.traces))

	blockID, err := i.CutBlockIfReady(0, 0, false)
	require.NoError(t, err, "unexpected error cutting block")
	require.NotEqual(t, blockID, uuid.Nil)

	err = i.CompleteBlock(blockID)
	require.NoError(t, err, "unexpected error completing block")

	block := i.GetBlockToBeFlushed(blockID)
	require.NotNil(t, block)
	require.Len(t, i.completingBlocks, 1)
	require.Len(t, i.completeBlocks, 1)

	err = ingester.store.WriteBlock(context.Background(), block)
	require.NoError(t, err)

	err = i.ClearFlushedBlocks(30 * time.Hour)
	require.NoError(t, err)
	require.Len(t, i.completeBlocks, 1)

	err = i.ClearFlushedBlocks(0)
	require.NoError(t, err)
	require.Len(t, i.completeBlocks, 0)

	err = i.resetHeadBlock()
	require.NoError(t, err, "unexpected error resetting block")

	require.Equal(t, int(i.traceCount.Load()), len(i.traces))
}

func TestInstanceFind(t *testing.T) {
	i, ingester := defaultInstance(t)

	numTraces := 10
	ids := [][]byte{}
	traces := []*tempopb.Trace{}
	for j := 0; j < numTraces; j++ {
		id := make([]byte, 16)
		rand.Read(id)

		testTrace := test.MakeTrace(10, id)
		trace.SortTrace(testTrace)
		traceBytes, err := model.MustNewSegmentDecoder(model.CurrentEncoding).PrepareForWrite(testTrace, 0, 0)
		require.NoError(t, err)

		err = i.PushBytes(context.Background(), id, traceBytes, nil)
		require.NoError(t, err)
		require.Equal(t, int(i.traceCount.Load()), len(i.traces))

		ids = append(ids, id)
		traces = append(traces, testTrace)
	}

	queryAll(t, i, ids, traces)

	err := i.CutCompleteTraces(0, true)
	require.NoError(t, err)
	require.Equal(t, int(i.traceCount.Load()), len(i.traces))

	for j := 0; j < numTraces; j++ {
		traceBytes, err := model.MustNewSegmentDecoder(model.CurrentEncoding).PrepareForWrite(traces[j], 0, 0)
		require.NoError(t, err)

		err = i.PushBytes(context.Background(), ids[j], traceBytes, nil)
		require.NoError(t, err)
	}

	queryAll(t, i, ids, traces)

	blockID, err := i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)
	require.NotEqual(t, blockID, uuid.Nil)

	queryAll(t, i, ids, traces)

	err = i.CompleteBlock(blockID)
	require.NoError(t, err)

	queryAll(t, i, ids, traces)

	err = i.ClearCompletingBlock(blockID)
	require.NoError(t, err)

	queryAll(t, i, ids, traces)

	localBlock := i.GetBlockToBeFlushed(blockID)
	require.NotNil(t, localBlock)

	err = ingester.store.WriteBlock(context.Background(), localBlock)
	require.NoError(t, err)

	queryAll(t, i, ids, traces)
}

func queryAll(t *testing.T, i *instance, ids [][]byte, traces []*tempopb.Trace) {
	for j, id := range ids {
		trace, err := i.FindTraceByID(context.Background(), id)
		require.NoError(t, err)
		require.Equal(t, traces[j], trace)
	}
}

func TestInstanceDoesNotRace(t *testing.T) {
	i, ingester := defaultInstance(t)
	end := make(chan struct{})

	concurrent := func(f func()) {
		for {
			select {
			case <-end:
				return
			default:
				f()
			}
		}
	}
	go concurrent(func() {
		request := makeRequest([]byte{})
		err := i.PushBytesRequest(context.Background(), request)
		require.NoError(t, err, "error pushing traces")
	})

	go concurrent(func() {
		err := i.CutCompleteTraces(0, true)
		require.NoError(t, err, "error cutting complete traces")
	})

	go concurrent(func() {
		blockID, _ := i.CutBlockIfReady(0, 0, false)
		if blockID != uuid.Nil {
			err := i.CompleteBlock(blockID)
			require.NoError(t, err, "unexpected error completing block")
			block := i.GetBlockToBeFlushed(blockID)
			require.NotNil(t, block)
			err = ingester.store.WriteBlock(context.Background(), block)
			require.NoError(t, err, "error writing block")
		}
	})

	go concurrent(func() {
		err := i.ClearFlushedBlocks(0)
		require.NoError(t, err, "error clearing flushed blocks")
	})

	go concurrent(func() {
		_, err := i.FindTraceByID(context.Background(), []byte{0x01})
		require.NoError(t, err, "error finding trace by id")
	})

	time.Sleep(100 * time.Millisecond)
	close(end)
	// Wait for go funcs to quit before
	// exiting and cleaning up
	time.Sleep(2 * time.Second)
}

func TestInstanceLimits(t *testing.T) {
	limits, err := overrides.NewOverrides(overrides.Limits{
		MaxBytesPerTrace:      1000,
		MaxLocalTracesPerUser: 4,
	})
	require.NoError(t, err, "unexpected error creating limits")
	limiter := NewLimiter(limits, &ringCountMock{count: 1}, 1)

	ingester, _, _ := defaultIngester(t, t.TempDir())

	type push struct {
		req          *tempopb.PushBytesRequest
		expectsError bool
	}

	tests := []struct {
		name   string
		pushes []push
	}{
		{
			name: "bytes - succeeds",
			pushes: []push{
				{
					req: makeRequestWithByteLimit(300, []byte{}),
				},
				{
					req: makeRequestWithByteLimit(500, []byte{}),
				},
				{
					req: makeRequestWithByteLimit(900, []byte{}),
				},
			},
		},
		{
			name: "bytes - one fails",
			pushes: []push{
				{
					req: makeRequestWithByteLimit(300, []byte{}),
				},
				{
					req:          makeRequestWithByteLimit(1500, []byte{}),
					expectsError: true,
				},
				{
					req: makeRequestWithByteLimit(900, []byte{}),
				},
			},
		},
		{
			name: "bytes - multiple pushes same trace",
			pushes: []push{
				{
					req: makeRequestWithByteLimit(500, []byte{0x01}),
				},
				{
					req:          makeRequestWithByteLimit(700, []byte{0x01}),
					expectsError: true,
				},
			},
		},
		{
			name: "max traces - too many",
			pushes: []push{
				{
					req: makeRequestWithByteLimit(100, []byte{}),
				},
				{
					req: makeRequestWithByteLimit(100, []byte{}),
				},
				{
					req: makeRequestWithByteLimit(100, []byte{}),
				},
				{
					req: makeRequestWithByteLimit(100, []byte{}),
				},
				{
					req:          makeRequestWithByteLimit(100, []byte{}),
					expectsError: true,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i, err := newInstance(testTenantID, limiter, ingester.store, ingester.local, false)
			require.NoError(t, err, "unexpected error creating new instance")

			for j, push := range tt.pushes {
				err = i.PushBytesRequest(context.Background(), push.req)

				require.Equalf(t, push.expectsError, err != nil, "push %d failed: %w", j, err)
			}
		})
	}
}

func TestInstanceCutCompleteTraces(t *testing.T) {
	id := make([]byte, 16)
	rand.Read(id)
	pastTrace := &liveTrace{
		traceID:    id,
		lastAppend: time.Now().Add(-time.Hour),
	}

	id = make([]byte, 16)
	rand.Read(id)
	nowTrace := &liveTrace{
		traceID:    id,
		lastAppend: time.Now().Add(time.Hour),
	}

	tt := []struct {
		name             string
		cutoff           time.Duration
		immediate        bool
		input            []*liveTrace
		expectedExist    []*liveTrace
		expectedNotExist []*liveTrace
	}{
		{
			name:      "empty",
			cutoff:    0,
			immediate: false,
		},
		{
			name:             "cut immediate",
			cutoff:           0,
			immediate:        true,
			input:            []*liveTrace{pastTrace, nowTrace},
			expectedNotExist: []*liveTrace{pastTrace, nowTrace},
		},
		{
			name:             "cut recent",
			cutoff:           0,
			immediate:        false,
			input:            []*liveTrace{pastTrace, nowTrace},
			expectedExist:    []*liveTrace{nowTrace},
			expectedNotExist: []*liveTrace{pastTrace},
		},
		{
			name:             "cut all time",
			cutoff:           2 * time.Hour,
			immediate:        false,
			input:            []*liveTrace{pastTrace, nowTrace},
			expectedNotExist: []*liveTrace{pastTrace, nowTrace},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			instance, _ := defaultInstance(t)

			for _, trace := range tc.input {
				fp := instance.tokenForTraceID(trace.traceID)
				instance.traces[fp] = trace
			}

			err := instance.CutCompleteTraces(tc.cutoff, tc.immediate)
			require.NoError(t, err)

			require.Equal(t, len(tc.expectedExist), len(instance.traces))
			for _, expectedExist := range tc.expectedExist {
				_, ok := instance.traces[instance.tokenForTraceID(expectedExist.traceID)]
				require.True(t, ok)
			}

			for _, expectedNotExist := range tc.expectedNotExist {
				_, ok := instance.traces[instance.tokenForTraceID(expectedNotExist.traceID)]
				require.False(t, ok)
			}
		})
	}
}

func TestInstanceCutBlockIfReady(t *testing.T) {
	tt := []struct {
		name               string
		maxBlockLifetime   time.Duration
		maxBlockBytes      uint64
		immediate          bool
		pushCount          int
		expectedToCutBlock bool
	}{
		{
			name:               "empty",
			expectedToCutBlock: false,
		},
		{
			name:               "doesnt cut anything",
			pushCount:          1,
			expectedToCutBlock: false,
		},
		{
			name:               "cut immediate",
			immediate:          true,
			pushCount:          1,
			expectedToCutBlock: true,
		},
		{
			name:               "cut based on block lifetime",
			maxBlockLifetime:   time.Microsecond,
			pushCount:          1,
			expectedToCutBlock: true,
		},
		{
			name:               "cut based on block size",
			maxBlockBytes:      10,
			pushCount:          10,
			expectedToCutBlock: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			instance, _ := defaultInstance(t)

			for i := 0; i < tc.pushCount; i++ {
				request := makeRequest([]byte{})
				err := instance.PushBytesRequest(context.Background(), request)
				require.NoError(t, err)
			}

			// Defaults
			if tc.maxBlockBytes == 0 {
				tc.maxBlockBytes = 1000
			}
			if tc.maxBlockLifetime == 0 {
				tc.maxBlockLifetime = time.Hour
			}

			lastCutTime := instance.lastBlockCut

			// Cut all traces to headblock for testing
			err := instance.CutCompleteTraces(0, true)
			require.NoError(t, err)

			blockID, err := instance.CutBlockIfReady(tc.maxBlockLifetime, tc.maxBlockBytes, tc.immediate)
			require.NoError(t, err)

			err = instance.CompleteBlock(blockID)
			if tc.expectedToCutBlock {
				require.NoError(t, err, "unexpected error completing block")
			}

			// Wait for goroutine to finish flushing to avoid test flakiness
			if tc.expectedToCutBlock {
				time.Sleep(time.Millisecond * 250)
			}

			require.Equal(t, tc.expectedToCutBlock, instance.lastBlockCut.After(lastCutTime))
		})
	}
}

func TestInstanceMetrics(t *testing.T) {
	i, _ := defaultInstance(t)
	cutAndVerify := func(v int) {
		err := i.CutCompleteTraces(0, true)
		require.NoError(t, err)

		liveTraces, err := test.GetGaugeVecValue(metricLiveTraces, testTenantID)
		require.NoError(t, err)
		require.Equal(t, v, int(liveTraces))
	}

	cutAndVerify(0)

	// Push some traces
	count := 100
	for j := 0; j < count; j++ {
		request := makeRequest([]byte{})
		err := i.PushBytesRequest(context.Background(), request)
		require.NoError(t, err)
	}
	cutAndVerify(count)
	cutAndVerify(0)
}

func TestInstanceFailsLargeTracesEvenAfterFlushing(t *testing.T) {
	ctx := context.Background()
	maxTraceBytes := 100
	id := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	ingester, _, _ := defaultIngester(t, t.TempDir())

	limits, err := overrides.NewOverrides(overrides.Limits{
		MaxBytesPerTrace: maxTraceBytes,
	})
	require.NoError(t, err)
	limiter := NewLimiter(limits, &ringCountMock{count: 1}, 1)

	i, err := newInstance(testTenantID, limiter, ingester.store, ingester.local, false)
	require.NoError(t, err)

	pushFn := func(byteCount int) error {
		return i.PushBytes(ctx, id, make([]byte, byteCount), nil)
	}

	// Fill up trace to max
	err = pushFn(maxTraceBytes)
	require.NoError(t, err)

	// Pushing again fails
	err = pushFn(3)
	require.Contains(t, err.Error(), (newTraceTooLargeError(id, i.instanceID, maxTraceBytes, 3)).Error())

	// Pushing still fails after flush
	err = i.CutCompleteTraces(0, true)
	require.NoError(t, err)
	err = pushFn(5)
	require.Contains(t, err.Error(), (newTraceTooLargeError(id, i.instanceID, maxTraceBytes, 5)).Error())

	// Cut block and then pushing works again
	_, err = i.CutBlockIfReady(0, 0, true)
	require.NoError(t, err)
	err = pushFn(maxTraceBytes)
	require.NoError(t, err)
}

func TestSortByteSlices(t *testing.T) {
	numTraces := 100

	// create first trace
	traceBytes := &tempopb.TraceBytes{
		Traces: make([][]byte, numTraces),
	}
	for i := range traceBytes.Traces {
		traceBytes.Traces[i] = make([]byte, rand.Intn(10))
		_, err := rand.Read(traceBytes.Traces[i])
		require.NoError(t, err)
	}

	// dupe
	traceBytes2 := &tempopb.TraceBytes{
		Traces: make([][]byte, numTraces),
	}
	for i := range traceBytes.Traces {
		traceBytes2.Traces[i] = make([]byte, len(traceBytes.Traces[i]))
		copy(traceBytes2.Traces[i], traceBytes.Traces[i])
	}

	// randomize dupe
	rand.Shuffle(len(traceBytes2.Traces), func(i, j int) {
		traceBytes2.Traces[i], traceBytes2.Traces[j] = traceBytes2.Traces[j], traceBytes2.Traces[i]
	})

	assert.NotEqual(t, traceBytes, traceBytes2)

	// sort and compare
	sortByteSlices(traceBytes.Traces)
	sortByteSlices(traceBytes2.Traces)

	assert.Equal(t, traceBytes, traceBytes2)
}

func defaultInstance(t testing.TB) (*instance, *Ingester) {
	instance, ingester, _ := defaultInstanceWithFlatBufferSearch(t, false)
	return instance, ingester
}

func defaultInstanceWithFlatBufferSearch(t testing.TB, fbSearch bool) (*instance, *Ingester, string) {
	limits, err := overrides.NewOverrides(overrides.Limits{})
	require.NoError(t, err, "unexpected error creating limits")
	limiter := NewLimiter(limits, &ringCountMock{count: 1}, 1)

	tmpDir := t.TempDir()

	ingester, _, _ := defaultIngester(t, tmpDir)
	instance, err := newInstance(testTenantID, limiter, ingester.store, ingester.local, fbSearch)
	require.NoError(t, err, "unexpected error creating new instance")

	return instance, ingester, tmpDir
}

func BenchmarkInstancePush(b *testing.B) {
	instance, _ := defaultInstance(b)
	request := makeRequest([]byte{})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Rotate trace ID
		binary.LittleEndian.PutUint32(request.Ids[0].Slice, uint32(i))
		err := instance.PushBytesRequest(context.Background(), request)
		require.NoError(b, err)
	}
}

func BenchmarkInstancePushExistingTrace(b *testing.B) {
	instance, _ := defaultInstance(b)
	request := makeRequest([]byte{})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := instance.PushBytesRequest(context.Background(), request)
		require.NoError(b, err)
	}
}

func BenchmarkInstanceFindTraceByID(b *testing.B) {
	instance, _ := defaultInstance(b)
	traceID := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	request := makeRequest(traceID)
	err := instance.PushBytesRequest(context.Background(), request)
	require.NoError(b, err)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		trace, err := instance.FindTraceByID(context.Background(), traceID)
		require.NotNil(b, trace)
		require.NoError(b, err)
	}
}

func makeRequest(traceID []byte) *tempopb.PushBytesRequest {
	const spans = 10

	traceID = test.ValidTraceID(traceID)
	return makePushBytesRequest(traceID, test.MakeBatch(spans, traceID))
}

// Note that this fn will generate a request with size **close to** maxBytes
func makeRequestWithByteLimit(maxBytes int, traceID []byte) *tempopb.PushBytesRequest {
	traceID = test.ValidTraceID(traceID)
	batch := test.MakeBatch(1, traceID)

	for batch.Size() < maxBytes {
		batch.InstrumentationLibrarySpans[0].Spans = append(batch.InstrumentationLibrarySpans[0].Spans, test.MakeSpan(traceID))
	}

	return makePushBytesRequest(traceID, batch)
}

func makePushBytesRequest(traceID []byte, batch *v1_trace.ResourceSpans) *tempopb.PushBytesRequest {
	trace := &tempopb.Trace{Batches: []*v1_trace.ResourceSpans{batch}}

	buffer, err := model.MustNewSegmentDecoder(model.CurrentEncoding).PrepareForWrite(trace, 0, 0)
	if err != nil {
		panic(err)
	}

	return &tempopb.PushBytesRequest{
		Ids: []tempopb.PreallocBytes{{
			Slice: traceID,
		}},
		Traces: []tempopb.PreallocBytes{{
			Slice: buffer,
		}},
	}
}
