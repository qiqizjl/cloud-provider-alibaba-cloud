package service

import (
	"fmt"
	"golang.org/x/net/context"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	queue "k8s.io/client-go/util/workqueue"
	"k8s.io/cloud-provider"
	"k8s.io/cloud-provider-alibaba-cloud/cloud-controller-manager/utils"
	"k8s.io/cloud-provider-alibaba-cloud/cloud-controller-manager/utils/metric"
	servicehelper "k8s.io/cloud-provider/service/helpers"
	metrics "k8s.io/component-base/metrics/prometheus/ratelimiter"
	"k8s.io/klog"
	controller "k8s.io/kube-aggregator/pkg/controllers"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const (
	//SERVICE_SYNC_PERIOD Interval of synchronizing service status from apiserver
	SERVICE_SYNC_PERIOD = 1 * time.Minute
	SERVICE_QUEUE       = "service-queue"
	SERVICE_CONTROLLER  = "service-controller"

	LabelNodeRoleMaster = "node-role.kubernetes.io/master"

	CCM_CLASS = "service.beta.kubernetes.io/class"
)

const TRY_AGAIN = "try again"

type Controller struct {
	cloud       cloudprovider.LoadBalancer
	client      clientset.Interface
	ifactory    informers.SharedInformerFactory
	clusterName string
	local       *Context
	caster      record.EventBroadcaster
	recorder    record.EventRecorder

	// Package workqueue provides a simple queue that supports the following
	// features:
	//  * Fair: items processed in the order in which they are added.
	//  * Stingy: a single item will not be processed multiple times concurrently,
	//      and if an item is added multiple times before it can be processed, it
	//      will only be processed once.
	//  * Multiple consumers and producers. In particular, it is allowed for an
	//      item to be reenqueued while it is being processed.
	//  * Shutdown notifications.
	queues map[string]queue.DelayingInterface
}

func NewController(
	cloud cloudprovider.LoadBalancer,
	client clientset.Interface,
	ifactory informers.SharedInformerFactory,
	clusterName string,
) (*Controller, error) {

	recorder, caster := broadcaster(client)
	rate := client.CoreV1().RESTClient().GetRateLimiter()
	if client != nil && rate != nil {
		if err := metrics.RegisterMetricAndTrackRateLimiterUsage("service_controller", rate); err != nil {
			return nil, err
		}
	}

	con := &Controller{
		cloud:       cloud,
		clusterName: clusterName,
		ifactory:    ifactory,
		local:       &Context{},
		caster:      caster,
		recorder:    recorder,
		client:      client,
		queues: map[string]queue.DelayingInterface{
			SERVICE_QUEUE: workqueue.NewNamedDelayingQueue(SERVICE_QUEUE),
		},
	}
	con.HandlerForEndpointChange(
		con.local,
		con.queues[SERVICE_QUEUE],
		con.ifactory.Core().V1().Endpoints().Informer(),
	)
	con.HandlerForNodesChange(
		con.local,
		con.queues[SERVICE_QUEUE],
		con.ifactory.Core().V1().Nodes().Informer(),
	)
	con.HandlerForServiceChange(
		con.local,
		con.queues[SERVICE_QUEUE],
		con.ifactory.Core().V1().Services().Informer(),
		recorder,
	)
	return con, nil
}

func (con *Controller) Run(stopCh <-chan struct{}, workers int) {
	defer runtime.HandleCrash()
	defer func() {
		for _, que := range con.queues {
			que.ShutDown()
		}
	}()

	klog.Info("starting service controller")
	defer klog.Info("shutting down service controller")

	if !controller.WaitForCacheSync(
		"service",
		stopCh,
		con.ifactory.Core().V1().Services().Informer().HasSynced,
		con.ifactory.Core().V1().Nodes().Informer().HasSynced,
	) {
		klog.Error("service and nodes cache has not been syncd")
		return
	}

	tasks := map[string]SyncTask{
		SERVICE_QUEUE: con.ServiceSyncTask,
	}
	for i := 0; i < workers; i++ {
		// run service sync worker
		klog.Infof("run service sync worker: %d", i)
		for que, task := range tasks {
			go wait.Until(
				WorkerFunc(
					con.local,
					con.queues[que],
					task,
				),
				2*time.Second,
				stopCh,
			)
		}
	}

	klog.Info("service controller started")
	<-stopCh
}

func broadcaster(client clientset.Interface) (record.EventRecorder, record.EventBroadcaster) {
	caster := record.NewBroadcaster()
	caster.StartLogging(klog.Infof)
	if client != nil {
		sink := &v1core.EventSinkImpl{
			Interface: v1core.New(client.CoreV1().RESTClient()).Events(""),
		}
		caster.StartRecordingToSink(sink)
	}
	source := v1.EventSource{Component: SERVICE_CONTROLLER}
	return caster.NewRecorder(scheme.Scheme, source), caster
}

func key(svc *v1.Service) string {
	return fmt.Sprintf("%s/%s", svc.Namespace, svc.Name)
}

func Enqueue(queue queue.DelayingInterface, k interface{}) {
	klog.Infof("controller: enqueue object %s for service, queue len %d", k.(string), queue.Len())
	queue.Add(k.(string))
}

func (con *Controller) HandlerForNodesChange(
	ctx *Context,
	que queue.DelayingInterface,
	informer cache.SharedIndexInformer,
) {

	syncNodes := func(object interface{}) {
		node, ok := object.(*v1.Node)
		if !ok || node == nil {
			klog.Info("node change: node object is nil, skip")
			return
		}
		if utils.IsExcludedNode(node) {
			klog.Infof("node change: node %s is excluded from CCM, skip", node.Name)
			return
		}
		// node change may affect any service that concerns
		// eg. Need LoadBalancer
		ctx.Range(
			func(k string, svc *v1.Service) bool {
				if !NeedLoadBalancer(svc) {
					utils.Logf(svc, "node change: loadbalancer is not needed, skip")
					return true
				}
				if !isProcessNeeded(svc) {
					utils.Logf(svc, "node change: class not empty, skip process ")
					return true
				}
				utils.Logf(svc, "node change: enqueue service")
				Enqueue(que, key(svc))
				return true
			},
		)
	}

	informer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: syncNodes,
			UpdateFunc: func(obja, objb interface{}) {
				node1, ok1 := obja.(*v1.Node)
				node2, ok2 := objb.(*v1.Node)
				if ok1 && ok2 &&
					NodeSpecChanged(node1, node2) {
					// label and schedulable changed .
					// status healthy should be considered
					klog.Infof("controller: node[%s/%s] update event", node1.Namespace, node1.Name)
					syncNodes(node1)
				}
			},
			DeleteFunc: syncNodes,
		},
		SERVICE_SYNC_PERIOD,
	)
}

func (con *Controller) HandlerForEndpointChange(
	ctx *Context,
	que queue.DelayingInterface,
	informer cache.SharedIndexInformer,
) {
	syncEndpoints := func(epd interface{}) {
		ep, ok := epd.(*v1.Endpoints)
		if !ok || ep == nil {
			klog.Info("endpoint change: endpoint object is nil, skip")
			return
		}
		svc := ctx.Get(fmt.Sprintf("%s/%s", ep.Namespace, ep.Name))
		if svc == nil {
			klog.Infof("endpoint change: can not get cached service for "+
				"endpoints[%s/%s], enqueue for default endpoint.\n", ep.Namespace, ep.Name)
			var err error
			svc, err = con.client.CoreV1().Services(ep.Namespace).Get(context.Background(), ep.Name, v12.GetOptions{})
			if err != nil {
				klog.Warningf("can not get service %s/%s. ", ep.Namespace, ep.Name)
				return
			}
		}
		if !isProcessNeeded(svc) {
			utils.Logf(svc, "endpoint: class not empty, skip process ")
			return
		}
		if !NeedLoadBalancer(svc) {
			// we are safe here to skip process syncEnpoint.
			utils.Logf(svc, "endpoint change: loadBalancer is not needed, skip")
			return
		}

		var epMsg []string
		for _, sub := range ep.Subsets {
			for _, add := range sub.Addresses {
				nodeName := ""
				if add.NodeName != nil {
					nodeName = *add.NodeName
				}
				epMsg = append(epMsg, fmt.Sprintf("ip: %s, nodeName: %s", add.IP, nodeName))
			}
		}
		utils.Logf(svc, "enqueue endpoint: %v", epMsg)

		Enqueue(que, key(svc))
	}
	informer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: syncEndpoints,
			UpdateFunc: func(obja, objb interface{}) {
				ep1, ok1 := obja.(*v1.Endpoints)
				ep2, ok2 := objb.(*v1.Endpoints)
				if ok1 && ok2 && !reflect.DeepEqual(ep1.Subsets, ep2.Subsets) {
					klog.Infof("controller: endpoints update event, endpoints [%s/%s]", ep1.Namespace, ep1.Name)
					syncEndpoints(ep2)
				}
			},
			DeleteFunc: syncEndpoints,
		},
		SERVICE_SYNC_PERIOD,
	)
}

func (con *Controller) HandlerForServiceChange(
	context *Context,
	que queue.DelayingInterface,
	informer cache.SharedIndexInformer,
	record record.EventRecorder,
) {
	syncService := func(svc *v1.Service) {
		if !isProcessNeeded(svc) {
			utils.Logf(svc, "class not empty, skip process")
			return
		}
		Enqueue(que, key(svc))
	}

	informer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(add interface{}) {
				svc, ok := add.(*v1.Service)
				if ok && NeedAdd(svc) {
					utils.Logf(svc, "controller: service addition event")
					syncService(svc)
				}
			},
			UpdateFunc: func(old, cur interface{}) {
				oldd, ok1 := old.(*v1.Service)
				curr, ok2 := cur.(*v1.Service)
				if ok1 && ok2 &&
					NeedUpdate(oldd, curr, record) {
					utils.Logf(curr, "controller: service update event")
					syncService(curr)
				}
			},
			DeleteFunc: func(cur interface{}) {
				svc, ok := cur.(*v1.Service)
				if ok && NeedDelete(svc) {
					utils.Logf(svc, "controller: service deletion received, %s", utils.PrettyJson(svc))
					// recorder service in local context
					context.Set(key(svc), svc)
					syncService(svc)
				}
			},
		},
		SERVICE_SYNC_PERIOD,
	)
}

func WorkerFunc(
	contex *Context,
	queue queue.DelayingInterface,
	syncd SyncTask,
) func() {

	return func() {
		// requeue exponential in case of throttle error.
		// for each worker function
		back := NewBackoff(5*time.Second, 1.5)
		for {
			func() {
				// Workerqueue ensures that a single key would not be process
				// by two worker concurrently, so multiple workers is safe here.
				key, quit := queue.Get()
				if quit {
					return
				}
				defer queue.Done(key)

				klog.Infof("[%s] worker: queued sync for service", key)

				if err := syncd(key.(string)); err != nil {
					if strings.Contains(err.Error(), "Throttling") {
						next := back.Next()
						queue.AddAfter(key, next)
						klog.Warningf("request was throttled: %s, retry in next %d ns", key, next)
					} else {
						queue.AddAfter(key, 5*time.Second)
					}
					klog.Errorf("requeue: sync error for service %s %v", key, err)
				}
			}()
		}
	}
}

func NewBackoff(
	next time.Duration,
	factor float64,
) *RequeueBackoff {
	r := &RequeueBackoff{
		last:   time.Now(),
		next:   next,
		factor: factor,
	}
	r.reset()
	return r
}

type RequeueBackoff struct {
	last   time.Time
	next   time.Duration
	factor float64 // next is multiplied by factor each iteration
}

func (b *RequeueBackoff) reset() {

	go wait.Until(
		func() {
			tick := time.NewTicker(20 * time.Second)
			for {
				// reset next retry interval when throttle ended.
				<-tick.C
				if time.Now().After(
					b.last.Add(1 * time.Minute),
				) {
					// throttle was last seen 1 minute ago.
					// reset next to 4 seconds
					b.next = 5 * time.Second
				}
			}
		},
		2*time.Second,
		make(chan struct{}),
	)
}

func (b *RequeueBackoff) Next() time.Duration {
	b.last = time.Now()
	b.next = time.Duration(float64(b.next) * b.factor)
	if b.next > 120*time.Second {
		// reset base to 30 seconds when throttle continued
		b.next = 30 * time.Second
	}
	return b.next
}

type SyncTask func(key string) error

// ------------------------------------------------- Where sync begins ---------------------------------------------------------

// SyncService Entrance for syncing service
func (con *Controller) ServiceSyncTask(k string) error {
	startTime := time.Now()

	ns, name, err := cache.SplitMetaNamespaceKey(k)
	if err != nil {
		return fmt.Errorf("unexpected key format %s for syncing service", k)
	}

	// local cache might be nil on first process which is expected
	cached := con.local.Get(k)

	defer func() {
		metric.SLBLatency.WithLabelValues("reconcile").Observe(metric.MsSince(startTime))
		klog.Infof("[%s] finished syncing service (%v)", k, time.Since(startTime))
	}()

	// service holds the latest service info from apiserver
	service, err := con.
		ifactory.
		Core().
		V1().
		Services().
		Lister().
		Services(ns).
		Get(name)
	switch {
	case errors.IsNotFound(err):

		if cached == nil {
			klog.Errorf("unexpected nil cached service for deletion, wait retry %s", k)
			return nil
		}
		// service absence in store means watcher caught the deletion, ensure LB
		// info is cleaned delete error would cause ReEnqueue svc, which mean retry.
		utils.Logf(cached, "service has been deleted %v", key(cached))
		return retry(nil, con.delete, cached)
	case err != nil:
		return fmt.Errorf("failed to load service from local context: %s", err.Error())
	default:
		// catch unexpected service
		if service == nil {
			klog.Errorf("unexpected nil service for update, wait retry. %s", k)
			return fmt.Errorf("retry unexpected nil service %s. ", k)
		}
		return con.update(cached, service)
	}
}

func isProcessNeeded(svc *v1.Service) bool { return svc.Annotations[CCM_CLASS] == "" }

func retry(
	backoff *wait.Backoff,
	fun func(svc *v1.Service) error,
	svc *v1.Service,
) error {
	if backoff == nil {
		backoff = &wait.Backoff{
			Duration: 1 * time.Second,
			Steps:    8,
			Factor:   2,
			Jitter:   4,
		}
	}
	return wait.ExponentialBackoff(
		*backoff,
		func() (bool, error) {
			err := fun(svc)
			if err != nil &&
				strings.Contains(err.Error(), TRY_AGAIN) {
				klog.Errorf("retry with error: %s", err.Error())
				return false, nil
			}
			if err != nil {
				klog.Errorf("retry error: NotRetry, %s", err.Error())
			}
			return true, nil
		},
	)
}

func (con *Controller) update(cached, svc *v1.Service) error {

	// Save the state so we can avoid a write if it doesn't change
	pre := svc.Status.LoadBalancer.DeepCopy()
	if cached != nil && cached.UID != svc.UID {
		klog.Warningf("UIDChanged,uid: %s -> %s, try delete old service first", cached.UID, svc.UID)
		return retry(nil, con.delete, svc)
	}
	ctx := context.Background()
	var newm *v1.LoadBalancerStatus
	if !NeedLoadBalancer(svc) {
		_, exits, err := con.cloud.GetLoadBalancer(ctx, "", svc)
		if err != nil {
			return err
		}
		if exits {
			// delete loadbalancer which is no longer needed
			utils.Logf(svc, "try delete loadbalancer which no longer needed for service.")
			if err := retry(nil, con.delete, svc); err != nil {
				return err
			}
		} else {
			// remove svc from cache which is not loadbalancer type
			con.local.Remove(key(svc))
		}

		//remove hashLabel
		if err := con.removeServiceHash(svc); err != nil {
			return err
		}

		// continue for updating service status.
		newm = &v1.LoadBalancerStatus{}
	} else {
		utils.Logf(svc, "start to ensure loadbalancer")
		start := time.Now()
		nodes, err := AvailableNodes(svc, con.ifactory)
		if err != nil {
			return fmt.Errorf("error get available nodes %s", err.Error())
		}
		// Fire warning event if there are no available nodes
		// for loadbalancer service
		if len(nodes) == 0 {
			con.recorder.Eventf(
				svc,
				v1.EventTypeWarning,
				"UnAvailableLoadBalancer",
				"There are no available nodes for LoadBalancer",
			)
		}
		ctx = context.WithValue(ctx, utils.ContextService, svc)
		ctx = context.WithValue(ctx, utils.ContextRecorder, con.recorder)
		newm, err = con.cloud.EnsureLoadBalancer(ctx, con.clusterName, svc, nodes)

		metric.SLBLatency.WithLabelValues("create").Observe(metric.MsSince(start))
		if err == nil {
			con.recorder.Eventf(
				svc,
				v1.EventTypeNormal,
				"EnsuredLoadBalancer",
				"Ensured load balancer",
			)
			if err := con.addServiceHash(svc); err != nil {
				return err
			}
		} else {
			message := getLogMessage(err)
			con.recorder.Eventf(
				svc,
				v1.EventTypeWarning,
				"SyncLoadBalancerFailed",
				"Error syncing load balancer: %s",
				message,
			)
			return fmt.Errorf("ensure loadbalancer error: %s", err)
		}
	}
	if err := con.updateStatus(svc, pre, newm); err != nil {
		return fmt.Errorf("update service status: %s", err.Error())
	}
	// Always update the cache upon success.
	// NOTE: Since we update the cached service if and only if we successfully
	// processed it, a cached service being nil implies that it hasn't yet
	// been successfully processed.
	con.local.Set(key(svc), svc)
	return nil
}

func (con *Controller) addServiceHash(svc *v1.Service) error {
	updated := svc.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = make(map[string]string)
	}
	serviceHash, err := utils.GetServiceHash(svc)
	if err != nil {
		return fmt.Errorf("compute service hash: %s", err.Error())
	}
	updated.Labels[utils.LabelServiceHash] = serviceHash
	if _, err := servicehelper.PatchService(con.client.CoreV1(), svc, updated); err != nil {
		return fmt.Errorf("update service hash: %s", err.Error())
	}
	return nil
}

func (con *Controller) removeServiceHash(svc *v1.Service) error {
	updated := svc.DeepCopy()
	if _, ok := updated.Labels[utils.LabelServiceHash]; ok {
		delete(updated.Labels, utils.LabelServiceHash)
		if _, err := servicehelper.PatchService(con.client.CoreV1(), svc, updated); err != nil {
			return fmt.Errorf("remove service hash, error: %s", err.Error())
		}
	}
	return nil
}

func (con *Controller) updateStatus(svc *v1.Service, pre, newm *v1.LoadBalancerStatus) error {
	if newm == nil {
		return fmt.Errorf("status not updated for nil status reason")
	}
	// Write the state if changed
	// TODO: Be careful here ... what if there were other changes to the service?
	if !v1helper.LoadBalancerStatusEqual(pre, newm) {
		utils.Logf(svc, "status: [%v] [%v]", pre, newm)
		return retry(
			&wait.Backoff{
				Duration: 1 * time.Second,
				Steps:    3,
				Factor:   2,
				Jitter:   4,
			},
			func(svc *v1.Service) error {
				// get latest svc from the shared informer cache
				updated, err := con.client.CoreV1().Services(svc.Namespace).Get(context.TODO(), svc.Name, metav1.GetOptions{ResourceVersion: "0"})
				if err != nil {
					return fmt.Errorf("error to get svc %s", key(svc))
				}
				updated.Status.LoadBalancer = *newm
				_, err = con.
					client.
					CoreV1().
					Services(updated.Namespace).
					UpdateStatus(context.Background(), updated, metav1.UpdateOptions{})
				if err == nil {
					return nil
				}
				// If the object no longer exists, we don't want to recreate it. Just bail
				// out so that we can process the delete, which we should soon be receiving
				// if we haven't already.
				if errors.IsNotFound(err) {
					utils.Logf(svc, "not persisting update to service that no "+
						"longer exists: %v", err)
					return nil
				}
				// TODO: Try to resolve the conflict if the change was unrelated to load
				// balancer status. For now, just pass it up the stack.
				if errors.IsConflict(err) {
					return fmt.Errorf("not persisting update to service %s that "+
						"has been changed since we received it: %v", key(svc), err)
				}
				klog.Warningf("failed to persist updated LoadBalancerStatus to "+
					"service %s after creating its load balancer: %v", key(svc), err)
				return fmt.Errorf("retry with %s, %s", err.Error(), TRY_AGAIN)
			},
			svc,
		)
	}
	utils.Logf(svc, "not persisting unchanged LoadBalancerStatus for service to registry.")
	return nil
}

func (con *Controller) delete(svc *v1.Service) error {
	ctx := context.Background()
	ctx = context.WithValue(ctx, utils.ContextService, svc)
	// do not check for the neediness of loadbalancer, delete anyway.
	klog.Infof("DeletingLoadBalancer for service %s", key(svc))

	start := time.Now()
	err := con.cloud.EnsureLoadBalancerDeleted(ctx, con.clusterName, svc)
	if err != nil {
		message := getLogMessage(err)
		con.recorder.Eventf(
			svc,
			v1.EventTypeWarning,
			"DeleteLoadBalancerFailed",
			"Error deleting load balancer: %s",
			message,
		)
		return fmt.Errorf(TRY_AGAIN)
	}
	metric.SLBLatency.WithLabelValues("delete").Observe(metric.MsSince(start))
	con.recorder.Eventf(
		svc,
		v1.EventTypeNormal,
		"DeletedLoadBalancer",
		"LoadBalancer Deleted SUCCESS. %s",
		key(svc),
	)
	con.local.Remove(key(svc))
	return nil
}

func AvailableNodes(
	svc *v1.Service,
	ifactory informers.SharedInformerFactory,
) ([]*v1.Node, error) {
	predicate, err := NodeConditionPredicate(svc)
	if err != nil {
		return nil, fmt.Errorf("error get predicate: %s", err.Error())
	}
	nodes, err := ifactory.
		Core().V1().Nodes().
		Lister().List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var filtered []*v1.Node
	for i := range nodes {
		if predicate(nodes[i]) {
			filtered = append(filtered, nodes[i])
		}
	}

	return filtered, nil
}

type NodeConditionPredicateFunc func(node *v1.Node) bool

func NodeConditionPredicate(svc *v1.Service) (NodeConditionPredicateFunc, error) {

	predicate := func(node *v1.Node) bool {
		// Filter unschedulable node.
		if node.Spec.Unschedulable {
			if svc.Annotations[utils.ServiceAnnotationLoadBalancerRemoveUnscheduledBackend] == "on" {
				utils.Logf(svc, "ignore node %s with unschedulable condition", node.Name)
				return false
			}
		}

		// As of 1.6, we will taint the master, but not necessarily mark
		// it unschedulable. Recognize nodes labeled as master, and filter
		// them also, as we were doing previously.
		if _, isMaster := node.Labels[LabelNodeRoleMaster]; isMaster {
			utils.Logf(svc, "svc %v check node role  %v ",svc.Name, node.Name)
			if svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyTypeCluster {
				utils.Logf(svc, "ignoring master node %v condition check", node.Name)
				return false
			}
		}

		// ignore eci node condition check
		if label, ok := node.Labels["type"]; ok && label == utils.ECINodeLabel {
			utils.Logf(svc, "ignoring eci node %v condition check", node.Name)
			return true
		}

		// If we have no info, don't accept
		if len(node.Status.Conditions) == 0 {
			return false
		}
		for _, cond := range node.Status.Conditions {
			// We consider the node for load balancing only when its NodeReady
			// condition status is ConditionTrue
			if cond.Type == v1.NodeReady &&
				cond.Status != v1.ConditionTrue {
				utils.Logf(svc, "ignoring node %v with %v condition "+
					"status %v", node.Name, cond.Type, cond.Status)
				return false
			}
		}

		return true
	}

	return predicate, nil
}

var re = regexp.MustCompile(".*(Message:.*)")

func getLogMessage(err error) string {
	var message string
	sub := re.FindSubmatch([]byte(err.Error()))
	if len(sub) > 1 {
		message = string(sub[1])
	} else {
		message = err.Error()
	}
	return message
}
