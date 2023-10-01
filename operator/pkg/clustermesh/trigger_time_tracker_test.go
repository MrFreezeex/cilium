package clustermesh

import (
	"runtime"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

var (
	t0 = time.Date(2019, 01, 01, 0, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Second)
	t2 = t1.Add(time.Second)
	t3 = t2.Add(time.Second)
	t4 = t3.Add(time.Second)
	t5 = t4.Add(time.Second)

	ttClusterName = "ttCluster1"
	ttNamespace   = "ttNamespace1"
	ttServiceName = "my-service"
	ttKey         = types.NamespacedName{Namespace: ttNamespace, Name: ttServiceName}
)

func TestNewServiceExistingSync(t *testing.T) {
	tester := newTester(t)

	service := createService(ttNamespace, ttServiceName, t3)
	tester.whenComputeEndpointLastChangeTriggerTime(ttKey, service, ttClusterName, t5).
		// Endpoint were synced were synced before service was created, but trigger time is the time when service was created.
		expect(t3)
}

func TestNewSync(t *testing.T) {
	tester := newTester(t)

	service := createService(ttNamespace, ttServiceName, t0)
	tester.whenComputeEndpointLastChangeTriggerTime(ttKey, service, ttClusterName, t0).expect(t0)

	tester.whenComputeEndpointLastChangeTriggerTime(ttKey, service, ttClusterName, t1).expect(t1)
}

func TestUpdatedNoOp(t *testing.T) {
	tester := newTester(t)

	service := createService(ttNamespace, ttServiceName, t0)
	tester.whenComputeEndpointLastChangeTriggerTime(ttKey, service, ttClusterName, t0).expect(t0)

	// Nothing has changed.
	tester.whenComputeEndpointLastChangeTriggerTime(ttKey, service, ttClusterName, t0).expectNil()
}

func TestServiceDeletedThenAdded(t *testing.T) {
	tester := newTester(t)

	service := createService(ttNamespace, ttServiceName, t0)
	tester.whenComputeEndpointLastChangeTriggerTime(ttKey, service, ttClusterName, t2).expect(t0)

	tester.DeleteService(ttKey)

	service = createService(ttNamespace, ttServiceName, t3)
	tester.whenComputeEndpointLastChangeTriggerTime(ttKey, service, ttClusterName, t2).expect(t3)
}

func TestServiceUpdatedNoChange(t *testing.T) {
	tester := newTester(t)

	service := createService(ttNamespace, ttServiceName, t0)
	tester.whenComputeEndpointLastChangeTriggerTime(ttKey, service, ttClusterName, t2).expect(t0)

	// service's ports have changed.
	service.Spec = v1.ServiceSpec{
		Selector: map[string]string{},
		Ports:    []v1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: "TCP"}},
	}

	// Currently we're not able to calculate trigger time for service updates, hence the returned
	// value is a nil time.
	tester.whenComputeEndpointLastChangeTriggerTime(ttKey, service, ttClusterName, t2).expectNil()
}

// ------- Test Utils -------

type tester struct {
	*TriggerTimeTracker
	t *testing.T
}

func newTester(t *testing.T) *tester {
	return &tester{NewTriggerTimeTracker(), t}
}

func (t *tester) whenComputeEndpointLastChangeTriggerTime(
	key types.NamespacedName, service *v1.Service, cluster string, clusterSyncedTime time.Time) subject {
	return subject{t.ComputeEndpointLastChangeTriggerTime(key, service, cluster, clusterSyncedTime), t.t}
}

type subject struct {
	got time.Time
	t   *testing.T
}

func (s subject) expect(expected time.Time) {
	s.doExpect(expected)
}

func (s subject) expectNil() {
	s.doExpect(time.Time{})
}

func (s subject) doExpect(expected time.Time) {
	if s.got != expected {
		_, fn, line, _ := runtime.Caller(2)
		s.t.Errorf("Wrong trigger time in %s:%d expected %s, got %s", fn, line, expected, s.got)
	}
}

func createService(namespace, ttServiceName string, creationTime time.Time) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              ttServiceName,
			CreationTimestamp: metav1.NewTime(creationTime),
		},
	}
}
