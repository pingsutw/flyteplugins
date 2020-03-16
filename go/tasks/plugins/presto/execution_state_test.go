package presto

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/core"
	"github.com/lyft/flyteplugins/go/tasks/plugins/presto/client"
	prestoMocks "github.com/lyft/flyteplugins/go/tasks/plugins/presto/client/mocks"
	"github.com/lyft/flyteplugins/go/tasks/plugins/presto/config"
	mocks2 "github.com/lyft/flytestdlib/cache/mocks"
	stdConfig "github.com/lyft/flytestdlib/config"
	"github.com/lyft/flytestdlib/contextutils"
	"github.com/lyft/flytestdlib/promutils"
	"github.com/lyft/flytestdlib/promutils/labeled"

	"github.com/lyft/flyteidl/gen/pb-go/flyteidl/plugins"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/core/mocks"
	pluginsCoreMocks "github.com/lyft/flyteplugins/go/tasks/pluginmachinery/core/mocks"
)

func init() {
	labeled.SetMetricKeys(contextutils.NamespaceKey)
}

func TestInTerminalState(t *testing.T) {
	var stateTests = []struct {
		phase      ExecutionPhase
		isTerminal bool
	}{
		{phase: PhaseNotStarted, isTerminal: false},
		{phase: PhaseQueued, isTerminal: false},
		{phase: PhaseSubmitted, isTerminal: false},
		{phase: PhaseQuerySucceeded, isTerminal: true},
		{phase: PhaseQueryFailed, isTerminal: true},
	}

	for _, tt := range stateTests {
		t.Run(tt.phase.String(), func(t *testing.T) {
			e := ExecutionState{Phase: tt.phase}
			res := InTerminalState(e)
			assert.Equal(t, tt.isTerminal, res)
		})
	}
}

func TestIsNotYetSubmitted(t *testing.T) {
	var stateTests = []struct {
		phase             ExecutionPhase
		isNotYetSubmitted bool
	}{
		{phase: PhaseNotStarted, isNotYetSubmitted: true},
		{phase: PhaseQueued, isNotYetSubmitted: true},
		{phase: PhaseSubmitted, isNotYetSubmitted: false},
		{phase: PhaseQuerySucceeded, isNotYetSubmitted: false},
		{phase: PhaseQueryFailed, isNotYetSubmitted: false},
	}

	for _, tt := range stateTests {
		t.Run(tt.phase.String(), func(t *testing.T) {
			e := ExecutionState{Phase: tt.phase}
			res := IsNotYetSubmitted(e)
			assert.Equal(t, tt.isNotYetSubmitted, res)
		})
	}
}

func TestGetQueryInfo(t *testing.T) {
	ctx := context.Background()

	taskTemplate := GetSingleHiveQueryTaskTemplate()
	mockTaskReader := &mocks.TaskReader{}
	mockTaskReader.On("Read", mock.Anything).Return(&taskTemplate, nil)

	mockTaskExecutionContext := mocks.TaskExecutionContext{}
	mockTaskExecutionContext.On("TaskReader").Return(mockTaskReader)

	taskMetadata := &pluginsCoreMocks.TaskExecutionMetadata{}
	taskMetadata.On("GetNamespace").Return("myproject-staging")
	taskMetadata.On("GetLabels").Return(map[string]string{"sample": "label"})
	mockTaskExecutionContext.On("TaskExecutionMetadata").Return(taskMetadata)

	routingGroup, catalog, schema, statement, err := GetQueryInfo(ctx, &mockTaskExecutionContext)
	assert.NoError(t, err)
	assert.Equal(t, "adhoc", routingGroup)
	assert.Equal(t, "hive", catalog)
	assert.Equal(t, "city", schema)
	assert.Equal(t, "select * from hive.city.fact_airport_sessions limit 10", statement)
}

func TestValidatePrestoStatement(t *testing.T) {
	prestoQuery := plugins.PrestoQuery{
		RoutingGroup: "adhoc",
		Catalog:      "hive",
		Schema:       "city",
		Statement:    "",
	}
	err := validatePrestoStatement(prestoQuery)
	assert.Error(t, err)
}

func TestConstructTaskLog(t *testing.T) {
	expected := "https://prestoproxy-internal.lyft.net:443"
	u, err := url.Parse(expected)
	assert.NoError(t, err)
	taskLog := ConstructTaskLog(ExecutionState{CommandID: "123", URI: u.String()})
	assert.Equal(t, expected, taskLog.Uri)
}

func TestConstructTaskInfo(t *testing.T) {
	empty := ConstructTaskInfo(ExecutionState{})
	assert.Nil(t, empty)

	expected := "https://prestoproxy-internal.lyft.net:443"
	u, err := url.Parse(expected)
	assert.NoError(t, err)

	e := ExecutionState{
		Phase:            PhaseQuerySucceeded,
		CommandID:        "123",
		SyncFailureCount: 0,
		URI:              u.String(),
	}

	taskInfo := ConstructTaskInfo(e)
	assert.Equal(t, "https://prestoproxy-internal.lyft.net:443", taskInfo.Logs[0].Uri)
}

func TestMapExecutionStateToPhaseInfo(t *testing.T) {
	t.Run("NotStarted", func(t *testing.T) {
		e := ExecutionState{
			Phase: PhaseNotStarted,
		}
		phaseInfo := MapExecutionStateToPhaseInfo(e)
		assert.Equal(t, core.PhaseNotReady, phaseInfo.Phase())
	})

	t.Run("Queued", func(t *testing.T) {
		e := ExecutionState{
			Phase:                PhaseQueued,
			CreationFailureCount: 0,
		}
		phaseInfo := MapExecutionStateToPhaseInfo(e)
		assert.Equal(t, core.PhaseRunning, phaseInfo.Phase())

		e = ExecutionState{
			Phase:                PhaseQueued,
			CreationFailureCount: 100,
		}
		phaseInfo = MapExecutionStateToPhaseInfo(e)
		assert.Equal(t, core.PhaseRetryableFailure, phaseInfo.Phase())

	})

	t.Run("Submitted", func(t *testing.T) {
		e := ExecutionState{
			Phase: PhaseSubmitted,
		}
		phaseInfo := MapExecutionStateToPhaseInfo(e)
		assert.Equal(t, core.PhaseRunning, phaseInfo.Phase())
	})
}

func TestGetAllocationToken(t *testing.T) {
	ctx := context.Background()

	t.Run("allocation granted", func(t *testing.T) {
		tCtx := GetMockTaskExecutionContext()
		mockResourceManager := tCtx.ResourceManager()
		x := mockResourceManager.(*mocks.ResourceManager)
		x.On("AllocateResource", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(core.AllocationStatusGranted, nil)

		mockCurrentState := ExecutionState{AllocationTokenRequestStartTime: time.Now()}
		mockMetrics := getPrestoExecutorMetrics(promutils.NewTestScope())
		state, err := GetAllocationToken(ctx, tCtx, mockCurrentState, mockMetrics)
		assert.NoError(t, err)
		assert.Equal(t, PhaseQueued, state.Phase)
	})

	t.Run("exhausted", func(t *testing.T) {
		tCtx := GetMockTaskExecutionContext()
		mockResourceManager := tCtx.ResourceManager()
		x := mockResourceManager.(*mocks.ResourceManager)
		x.On("AllocateResource", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(core.AllocationStatusExhausted, nil)

		mockCurrentState := ExecutionState{AllocationTokenRequestStartTime: time.Now()}
		mockMetrics := getPrestoExecutorMetrics(promutils.NewTestScope())
		state, err := GetAllocationToken(ctx, tCtx, mockCurrentState, mockMetrics)
		assert.NoError(t, err)
		assert.Equal(t, PhaseNotStarted, state.Phase)
	})

	t.Run("namespace exhausted", func(t *testing.T) {
		tCtx := GetMockTaskExecutionContext()
		mockResourceManager := tCtx.ResourceManager()
		x := mockResourceManager.(*mocks.ResourceManager)
		x.On("AllocateResource", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(core.AllocationStatusNamespaceQuotaExceeded, nil)

		mockCurrentState := ExecutionState{AllocationTokenRequestStartTime: time.Now()}
		mockMetrics := getPrestoExecutorMetrics(promutils.NewTestScope())
		state, err := GetAllocationToken(ctx, tCtx, mockCurrentState, mockMetrics)
		assert.NoError(t, err)
		assert.Equal(t, PhaseNotStarted, state.Phase)
	})

	t.Run("Request start time, if empty in current state, should be set", func(t *testing.T) {
		tCtx := GetMockTaskExecutionContext()
		mockResourceManager := tCtx.ResourceManager()
		x := mockResourceManager.(*mocks.ResourceManager)
		x.On("AllocateResource", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(core.AllocationStatusNamespaceQuotaExceeded, nil)

		mockCurrentState := ExecutionState{}
		mockMetrics := getPrestoExecutorMetrics(promutils.NewTestScope())
		state, err := GetAllocationToken(ctx, tCtx, mockCurrentState, mockMetrics)
		assert.NoError(t, err)
		assert.Equal(t, state.AllocationTokenRequestStartTime.IsZero(), false)
	})

	t.Run("Request start time, if already set in current state, should be maintained", func(t *testing.T) {
		tCtx := GetMockTaskExecutionContext()
		mockResourceManager := tCtx.ResourceManager()
		x := mockResourceManager.(*mocks.ResourceManager)
		x.On("AllocateResource", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
			Return(core.AllocationStatusGranted, nil)

		startTime := time.Now()
		mockCurrentState := ExecutionState{AllocationTokenRequestStartTime: startTime}
		mockMetrics := getPrestoExecutorMetrics(promutils.NewTestScope())
		state, err := GetAllocationToken(ctx, tCtx, mockCurrentState, mockMetrics)
		assert.NoError(t, err)
		assert.Equal(t, state.AllocationTokenRequestStartTime.IsZero(), false)
		assert.Equal(t, state.AllocationTokenRequestStartTime, startTime)
	})
}

func TestAbort(t *testing.T) {
	ctx := context.Background()

	t.Run("Terminate called when not in terminal state", func(t *testing.T) {
		var x = false

		mockPresto := &prestoMocks.PrestoClient{}
		mockPresto.On("KillCommand", mock.Anything, mock.MatchedBy(func(commandId string) bool {
			return commandId == "123456"
		}), mock.Anything).Run(func(_ mock.Arguments) {
			x = true
		}).Return(nil)

		err := Abort(ctx, ExecutionState{Phase: PhaseSubmitted, CommandID: "123456"}, mockPresto)
		assert.NoError(t, err)
		assert.True(t, x)
	})

	t.Run("Terminate not called when in terminal state", func(t *testing.T) {
		var x = false

		mockPresto := &prestoMocks.PrestoClient{}
		mockPresto.On("KillCommand", mock.Anything, mock.Anything).Run(func(_ mock.Arguments) {
			x = true
		}).Return(nil)

		err := Abort(ctx, ExecutionState{Phase: PhaseQuerySucceeded, CommandID: "123456"}, mockPresto)
		assert.NoError(t, err)
		assert.False(t, x)
	})
}

func TestFinalize(t *testing.T) {
	// Test that Finalize releases resources
	ctx := context.Background()
	tCtx := GetMockTaskExecutionContext()
	state := ExecutionState{}
	var called = false
	mockResourceManager := tCtx.ResourceManager()
	x := mockResourceManager.(*mocks.ResourceManager)
	x.On("ReleaseResource", mock.Anything, mock.Anything, mock.Anything).Run(func(_ mock.Arguments) {
		called = true
	}).Return(nil)

	err := Finalize(ctx, tCtx, state)
	assert.NoError(t, err)
	assert.True(t, called)
}

func TestMonitorQuery(t *testing.T) {
	ctx := context.Background()
	tCtx := GetMockTaskExecutionContext()
	state := ExecutionState{
		Phase: PhaseSubmitted,
	}
	var getOrCreateCalled = false
	mockCache := &mocks2.AutoRefresh{}
	mockCache.OnGetOrCreateMatch("my_wf_exec_project:my_wf_exec_domain:my_wf_exec_name", mock.Anything).Return(ExecutionStateCacheItem{
		ExecutionState: ExecutionState{Phase: PhaseQuerySucceeded},
		Identifier:     "my_wf_exec_project:my_wf_exec_domain:my_wf_exec_name",
	}, nil).Run(func(_ mock.Arguments) {
		getOrCreateCalled = true
	})

	newState, err := MonitorQuery(ctx, tCtx, state, mockCache)
	assert.NoError(t, err)
	assert.True(t, getOrCreateCalled)
	assert.Equal(t, PhaseQuerySucceeded, newState.Phase)
}

func TestKickOffQuery(t *testing.T) {
	ctx := context.Background()
	tCtx := GetMockTaskExecutionContext()

	var prestoCalled = false

	prestoExecuteResponse := client.PrestoExecuteResponse{
		ID:     "1234567",
		Status: client.PrestoStatusWaiting,
	}
	mockPresto := &prestoMocks.PrestoClient{}
	mockPresto.OnExecuteCommandMatch(mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		mock.Anything, mock.Anything).Run(func(_ mock.Arguments) {
		prestoCalled = true
	}).Return(prestoExecuteResponse, nil)
	var getOrCreateCalled = false
	mockCache := &mocks2.AutoRefresh{}
	mockCache.OnGetOrCreate(mock.Anything, mock.Anything).Run(func(_ mock.Arguments) {
		getOrCreateCalled = true
	}).Return(ExecutionStateCacheItem{}, nil)

	state := ExecutionState{}
	newState, err := KickOffQuery(ctx, tCtx, state, mockPresto, mockCache)
	assert.NoError(t, err)
	assert.Equal(t, PhaseSubmitted, newState.Phase)
	assert.Equal(t, "1234567", newState.CommandID)
	assert.True(t, getOrCreateCalled)
	assert.True(t, prestoCalled)
}

func createMockPrestoCfg() *config.Config {
	return &config.Config{
		Environment:         config.URLMustParse(""),
		DefaultRoutingGroup: "adhoc",
		RoutingGroupConfigs: []config.RoutingGroupConfig{{Name: "adhoc", Limit: 250}, {Name: "etl", Limit: 100}},
		RateLimiter: config.RateLimiter{
			Name:         "presto",
			SyncPeriod:   stdConfig.Duration{Duration: 3 * time.Second},
			Workers:      15,
			LruCacheSize: 2000,
			MetricScope:  "presto",
		},
	}
}

func Test_mapLabelToPrimaryLabel(t *testing.T) {
	ctx := context.TODO()
	mockPrestoCfg := createMockPrestoCfg()

	type args struct {
		ctx          context.Context
		routingGroup string
		prestoCfg    *config.Config
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{name: "Routing group is found in configs", args: args{ctx: ctx, routingGroup: "etl", prestoCfg: mockPrestoCfg}, want: "etl"},
		{name: "Use routing group default when not found in configs", args: args{ctx: ctx, routingGroup: "test", prestoCfg: mockPrestoCfg}, want: mockPrestoCfg.DefaultRoutingGroup},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveRoutingGroup(tt.args.ctx, tt.args.routingGroup, tt.args.prestoCfg))
		})
	}
}
