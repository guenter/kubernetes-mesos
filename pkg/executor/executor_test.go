package executor

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/dockertools"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/glog"
	bindings "github.com/mesos/mesos-go/executor"
	mesos "github.com/mesos/mesos-go/mesosproto"
	mutil "github.com/mesos/mesos-go/mesosutil"
	"github.com/mesosphere/kubernetes-mesos/pkg/executor/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

type suicideTracker struct {
	suicideWatcher
	stops  uint32
	resets uint32
	timers uint32
	jumps  *uint32
}

func (t *suicideTracker) Reset(d time.Duration) bool {
	defer func() { t.resets++ }()
	return t.suicideWatcher.Reset(d)
}

func (t *suicideTracker) Stop() bool {
	defer func() { t.stops++ }()
	return t.suicideWatcher.Stop()
}

func (t *suicideTracker) Next(d time.Duration, driver bindings.ExecutorDriver, f jumper) suicideWatcher {
	tracker := &suicideTracker{
		stops:  t.stops,
		resets: t.resets,
		jumps:  t.jumps,
		timers: t.timers + 1,
	}
	jumper := tracker.makeJumper(f)
	tracker.suicideWatcher = t.suicideWatcher.Next(d, driver, jumper)
	return tracker
}

func (t *suicideTracker) makeJumper(_ jumper) jumper {
	return jumper(func(driver bindings.ExecutorDriver, cancel <-chan struct{}) {
		glog.Warningln("jumping?!")
		if t.jumps != nil {
			atomic.AddUint32(t.jumps, 1)
		}
	})
}

type MockExecutorDriver struct {
	mock.Mock
}

func (m *MockExecutorDriver) Start() (mesos.Status, error) {
	args := m.Called()
	return args.Get(0).(mesos.Status), args.Error(1)
}

func (m *MockExecutorDriver) Stop() (mesos.Status, error) {
	args := m.Called()
	return args.Get(0).(mesos.Status), args.Error(1)
}

func (m *MockExecutorDriver) Abort() (mesos.Status, error) {
	args := m.Called()
	return args.Get(0).(mesos.Status), args.Error(1)
}

func (m *MockExecutorDriver) Join() (mesos.Status, error) {
	args := m.Called()
	return args.Get(0).(mesos.Status), args.Error(1)
}

func (m *MockExecutorDriver) Run() (mesos.Status, error) {
	args := m.Called()
	return args.Get(0).(mesos.Status), args.Error(1)
}

func (m *MockExecutorDriver) SendStatusUpdate(status *mesos.TaskStatus) (mesos.Status, error) {
	args := m.Called(status)
	return args.Get(0).(mesos.Status), args.Error(1)
}

func (m *MockExecutorDriver) SendFrameworkMessage(message string) (mesos.Status, error) {
	args := m.Called(message)
	return args.Get(0).(mesos.Status), args.Error(1)
}

func TestSuicide_zeroTimeout(t *testing.T) {
	defer glog.Flush()

	k := New(Config{})
	tracker := &suicideTracker{suicideWatcher: k.suicideWatch}
	k.suicideWatch = tracker

	ch := k.resetSuicideWatch(nil)

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for reset of suicide watch")
	}
	if tracker.stops != 0 {
		t.Fatalf("expected no stops since suicideWatchTimeout was never set")
	}
	if tracker.resets != 0 {
		t.Fatalf("expected no resets since suicideWatchTimeout was never set")
	}
	if tracker.timers != 0 {
		t.Fatalf("expected no timers since suicideWatchTimeout was never set")
	}
}

func TestSuicide_WithTasks(t *testing.T) {
	defer glog.Flush()

	k := New(Config{
		SuicideTimeout: 50 * time.Millisecond,
	})

	jumps := uint32(0)
	tracker := &suicideTracker{suicideWatcher: k.suicideWatch, jumps: &jumps}
	k.suicideWatch = tracker

	k.tasks["foo"] = &kuberTask{} // prevent suicide attempts from succeeding

	// call reset with a nil timer
	glog.Infoln("resetting suicide watch with 1 task")
	select {
	case <-k.resetSuicideWatch(nil):
		tracker = k.suicideWatch.(*suicideTracker)
		if tracker.stops != 1 {
			t.Fatalf("expected suicide attempt to Stop() since there are registered tasks")
		}
		if tracker.resets != 0 {
			t.Fatalf("expected no resets since")
		}
		if tracker.timers != 0 {
			t.Fatalf("expected no timers since")
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("initial suicide watch setup failed")
	}

	delete(k.tasks, "foo") // zero remaining tasks
	k.suicideTimeout = 1500 * time.Millisecond
	suicideStart := time.Now()

	// reset the suicide watch, which should actually start a timer now
	glog.Infoln("resetting suicide watch with 0 tasks")
	select {
	case <-k.resetSuicideWatch(nil):
		tracker = k.suicideWatch.(*suicideTracker)
		if tracker.stops != 1 {
			t.Fatalf("did not expect suicide attempt to Stop() since there are no registered tasks")
		}
		if tracker.resets != 1 {
			t.Fatalf("expected 1 resets instead of %d", tracker.resets)
		}
		if tracker.timers != 1 {
			t.Fatalf("expected 1 timers instead of %d", tracker.timers)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("2nd suicide watch setup failed")
	}

	k.lock.Lock()
	k.tasks["foo"] = &kuberTask{} // prevent suicide attempts from succeeding
	k.lock.Unlock()

	// reset the suicide watch, which should stop the existing timer
	glog.Infoln("resetting suicide watch with 1 task")
	select {
	case <-k.resetSuicideWatch(nil):
		tracker = k.suicideWatch.(*suicideTracker)
		if tracker.stops != 2 {
			t.Fatalf("expected 2 stops instead of %d since there are registered tasks", tracker.stops)
		}
		if tracker.resets != 1 {
			t.Fatalf("expected 1 resets instead of %d", tracker.resets)
		}
		if tracker.timers != 1 {
			t.Fatalf("expected 1 timers instead of %d", tracker.timers)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("3rd suicide watch setup failed")
	}

	k.lock.Lock()
	delete(k.tasks, "foo") // allow suicide attempts to schedule
	k.lock.Unlock()

	// reset the suicide watch, which should reset a stopped timer
	glog.Infoln("resetting suicide watch with 0 tasks")
	select {
	case <-k.resetSuicideWatch(nil):
		tracker = k.suicideWatch.(*suicideTracker)
		if tracker.stops != 2 {
			t.Fatalf("expected 2 stops instead of %d since there are no registered tasks", tracker.stops)
		}
		if tracker.resets != 2 {
			t.Fatalf("expected 2 resets instead of %d", tracker.resets)
		}
		if tracker.timers != 1 {
			t.Fatalf("expected 1 timers instead of %d", tracker.timers)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("4th suicide watch setup failed")
	}

	sinceWatch := time.Since(suicideStart)
	time.Sleep(3*time.Second - sinceWatch) // give the first timer to misfire (it shouldn't since Stop() was called)

	if j := atomic.LoadUint32(&jumps); j != 1 {
		t.Fatalf("expected 1 jumps instead of %d since stop was called", j)
	} else {
		glog.Infoln("jumps verified") // glog so we get a timestamp
	}
}

func TestRegistered(t *testing.T) {
	driver := new(MockExecutorDriver)
	fakeDocker := &dockertools.FakeDockerClient{}
	updates := make(chan interface{}, 10)
	config := Config{
		Docker:  fakeDocker,
		Updates: updates,
	}
	executorInfo := &mesos.ExecutorInfo{}
	frameworkInfo := &mesos.FrameworkInfo{}
	slaveInfo := &mesos.SlaveInfo{}

	k := New(config)
	k.Init(driver)
	k.Registered(driver, executorInfo, frameworkInfo, slaveInfo)

	assert.True(t, k.isConnected())
}

func TestLaunchTask(t *testing.T) {
	taskId := mutil.NewTaskID("task1")

	statusUpdate := &mesos.TaskStatus{
		TaskId:  taskId,
		State:   mesos.TaskState_TASK_STARTING.Enum(),
		Message: proto.String(messages.CreateBindingSuccess),
	}

	driver := new(MockExecutorDriver)
	driver.On("SendStatusUpdate", statusUpdate).Return(mesos.Status_DRIVER_RUNNING, nil)
	driver.On("Stop").Return(mesos.Status_DRIVER_STOPPED, nil)

	fakeDocker := &dockertools.FakeDockerClient{}
	config := Config{
		Docker:          fakeDocker,
		Updates:         make(chan interface{}, 10),
		KubeletFinished: make(chan struct{}),
	}
	executorInfo := &mesos.ExecutorInfo{}
	frameworkInfo := &mesos.FrameworkInfo{}
	slaveInfo := &mesos.SlaveInfo{}

	k := New(config)
	k.Init(driver)
	k.Registered(driver, executorInfo, frameworkInfo, slaveInfo)

	taskInfo := &mesos.TaskInfo{
		TaskId: taskId,
		Data: 
	}

	k.LaunchTask(driver, taskInfo)

	k.Shutdown(driver)

	driver.AssertExpectations(t)
}

func TestLaunchTask_notConnected(t *testing.T) {
	taskId := mutil.NewTaskID("task1")

	statusUpdate := &mesos.TaskStatus{
		TaskId:  taskId,
		State:   mesos.TaskState_TASK_FAILED.Enum(),
		Message: proto.String(messages.ExecutorUnregistered),
	}

	driver := new(MockExecutorDriver)
	driver.On("SendStatusUpdate", statusUpdate).Return(mesos.Status_DRIVER_RUNNING, nil)
	driver.On("Stop").Return(mesos.Status_DRIVER_STOPPED, nil)

	fakeDocker := &dockertools.FakeDockerClient{}
	config := Config{
		Docker:          fakeDocker,
		Updates:         make(chan interface{}, 10),
		KubeletFinished: make(chan struct{}),
	}

	k := New(config)
	k.Init(driver)

	taskInfo := &mesos.TaskInfo{
		TaskId: taskId,
	}

	k.LaunchTask(driver, taskInfo)

	k.Shutdown(driver)

	driver.AssertExpectations(t)
}
