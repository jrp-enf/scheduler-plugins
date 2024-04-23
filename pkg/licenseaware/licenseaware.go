package licenseaware

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/jrp-enf/license-operator/api/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/scheduler-plugins/apis/scheduling/v1alpha1"
)

type Reservation struct {
	podNamespace     string
	podName          string
	licenseNamespace string
	licenseName      string
	count            int
	expiration       time.Time
}

//+kubebuilder:rbac:groups=api.license-operator.enfabrica.net,resources=licenses,verbs=get;list

type LicenseAware struct {
	frameworkHandler framework.Handle
	defaultNamespace string
	client           client.Client
	mutex            sync.Mutex
	reservations     map[string][]Reservation
}

var _ framework.PreEnqueuePlugin = &LicenseAware{}
var _ framework.EnqueueExtensions = &LicenseAware{}

const (
	// Name is the name of the plugin used in Registry and configurations.
	Name = "LicenseAware"
)

// New initializes and returns a new LicenseAware plugin.
func New(obj runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	//	args, ok := obj.(*config.LicenseAwareArgs)
	//	if !ok {
	//		return nil, fmt.Errorf("want args to be of type LicenseAwareArgs, got %T", obj)
	//	}
	defaultNamespace := "default"
	//	if args.DefaultNamespace != "" {
	//		defaultNamespace = args.DefaultNamespace
	//	}

	scheme := runtime.NewScheme()
	_ = clientscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	_ = apiv1.AddToScheme(scheme)
	client, err := client.New(handle.KubeConfig(), client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}

	// Performance improvement when retrieving list of objects by namespace or we'll log 'index not exist' warning.
	handle.SharedInformerFactory().Core().V1().Pods().Informer().AddIndexers(cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

	plugin := &LicenseAware{
		frameworkHandler: handle,
		defaultNamespace: defaultNamespace,
		client:           client,
		reservations:     make(map[string][]Reservation),
	}
	return plugin, nil
}

func (l *LicenseAware) PreEnqueue(ctx context.Context, pod *v1.Pod) *framework.Status {
	klog.Info("In License Aware PreEnqueue")
	for k, v := range pod.Annotations {
		// See if the license object has licenses
		if k == "enfabrica.net/license" {
			klog.Info("Found pod has licenses")
			stuck := false
			var toReserve []Reservation
			for _, lic := range strings.Split(v, ",") {
				usage := 1
				val := strings.Split(lic, "/")
				if len(val) != 1 && len(val) != 2 {
					klog.Error(errors.New("unable to parse license for pod"), " namespace:", pod.Namespace, " name:", pod.Name, " license:", lic)
					continue
				}
				if len(val) == 2 {
					u, err := strconv.Atoi(val[1])
					if err != nil {
						klog.Error(errors.New("unable to parse license for pod"), " namespace:", pod.Namespace, " name:", pod.Name, " license:", lic)
						continue
					}
					usage = u
				}
				myLicense := &apiv1.License{}
				if err := l.client.Get(ctx, client.ObjectKey{Namespace: l.defaultNamespace, Name: val[0]}, myLicense); err != nil {
					klog.Error(errors.New("unable to retrieve license for pod"), err, " namespace:", pod.Namespace, " name:", pod.Name, " license:", val[0])
					continue
				}
				ra := myLicense.Status.RealAvailable
				hr := l.HasReserved(l.defaultNamespace, val[0], pod.Namespace, pod.Name, usage, myLicense.Status.UsedBy)
				klog.Info("Real available:", ra, " has_reserved:", hr, " usage:", usage)
				if ra-hr < usage {
					klog.Info("pod license unavailable", " namespace:", pod.Namespace, " name:", pod.Name, " license:", val[0])
					stuck = true
				} else {
					toReserve = append(toReserve, Reservation{
						podNamespace:     pod.Namespace,
						podName:          pod.Name,
						licenseNamespace: l.defaultNamespace,
						licenseName:      val[0],
						count:            usage,
						expiration:       time.Now().Add(5 * time.Minute),
					})
				}
			}
			if stuck {
				return framework.NewStatus(framework.Unschedulable, "Missing license")
			} else {
				// reserve and allow
				l.Reserve(toReserve)
				return framework.NewStatus(framework.Success, "")
			}
		}
	}

	return framework.NewStatus(framework.Success, "")
}

func (l *LicenseAware) Reserve(list []Reservation) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
nextInList:
	for _, r := range list {
		for _, res := range l.reservations[r.licenseNamespace+"::"+r.licenseName] {
			if res.podName == r.podName && res.podNamespace == r.podNamespace { // We've already got a reservation for it
				continue nextInList
			}
		}
		klog.Info("Reserving license: ", r.licenseNamespace+"::"+r.licenseName, " usage:", r.count)
		l.reservations[r.licenseNamespace+"::"+r.licenseName] = append(l.reservations[r.licenseNamespace+"::"+r.licenseName], r)
	}
}

func PodStatusEntry(namespace, name string, count int) string {
	return namespace + "::" + name + "/" + strconv.Itoa(count)
}

// slices.Contaiins not in this version of go
func contains(usedBy []string, entry string) bool {
	for _, v := range usedBy {
		if v == entry {
			return true
		}
	}
	return false
}
func (l *LicenseAware) HasReserved(namespace, name string, podNamespace, podName string, usage int, usedBy []string) int {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	reservedUsage := 0
	var reservationsToPersist []Reservation
	for _, v := range l.reservations[namespace+"::"+name] {
		if v.podName == podName && v.podNamespace == podNamespace {
			klog.Info("Pod already reserved usage, ignored, do not persist")
		} else {
			// if the entry is expired or it's in the list as being used, don't persist it
			if time.Now().After(v.expiration) || contains(usedBy, PodStatusEntry(v.podNamespace, v.podName, v.count)) {
				continue
			}
			reservationsToPersist = append(reservationsToPersist, v)
			reservedUsage = reservedUsage + v.count
		}
	}
	l.reservations[namespace+"::"+name] = reservationsToPersist
	return reservedUsage
}

// Name returns name of the plugin. It is used in logs, etc.
func (l *LicenseAware) Name() string {
	return Name
}

func (l *LicenseAware) EventsToRegister() []framework.ClusterEventWithHint {
	eqGVK := "licenses.v1.api.license-operator.enfabrica.net"
	return []framework.ClusterEventWithHint{
		{Event: framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.Delete}},
		{Event: framework.ClusterEvent{Resource: framework.GVK(eqGVK), ActionType: framework.All}},
		{Event: framework.ClusterEvent{Resource: framework.WildCard, ActionType: framework.All}},
	}

}
