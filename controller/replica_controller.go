package controllers

import (
	"fmt"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/controller"

	"github.com/rancher/longhorn-manager/types"

	longhorn "github.com/rancher/longhorn-manager/k8s/pkg/apis/longhorn/v1alpha1"
	lhclientset "github.com/rancher/longhorn-manager/k8s/pkg/client/clientset/versioned"
	lhinformers "github.com/rancher/longhorn-manager/k8s/pkg/client/informers/externalversions/longhorn/v1alpha1"
	lhlisters "github.com/rancher/longhorn-manager/k8s/pkg/client/listers/longhorn/v1alpha1"
)

var (
	// controllerKind contains the schema.GroupVersionKind for this controller type.
	controllerKind = longhorn.SchemeGroupVersion.WithKind("Replica")
)

const (
	// maxRetries is the number of times a deployment will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a deployment is going to be requeued:
	//
	// 5ms, 10ms, 20ms
	maxRetries = 3

	// longhornDirectory is the directory going to be bind mounted on the
	// host to provide storage space to replica data
	longhornDirectory = "/var/lib/rancher/longhorn/"

	// longhornReplicaKey is the key to identify which volume the replica
	// belongs to, for scheduling purpose
	longhornReplicaKey = "longhorn-volume-replica"
)

type ReplicaController struct {
	namespace string

	kubeClient    clientset.Interface
	eventRecorder record.EventRecorder
	podControl    controller.PodControlInterface

	lhClient lhclientset.Interface

	// To allow injection for testing
	syncHandler           func(rKey string) error
	enqueueReplicaHandler func(r *longhorn.Replica)
	updateReplicaHandler  func(r *longhorn.Replica) (*longhorn.Replica, error)

	rLister      lhlisters.ReplicaLister
	rStoreSynced cache.InformerSynced

	pLister      corelisters.PodLister
	pStoreSynced cache.InformerSynced

	queue workqueue.RateLimitingInterface
}

type Replica struct {
	longhorn.Replica
	namespace string
}

func NewReplicaController(replicaInformer lhinformers.ReplicaInformer, podInformer coreinformers.PodInformer, lhClient lhclientset.Interface, kubeClient clientset.Interface, namespace string) *ReplicaController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logrus.Infof)
	// TODO: remove the wrapper when every clients have moved to use the clientset.
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(kubeClient.CoreV1().RESTClient()).Events("")})

	rc := &ReplicaController{
		namespace: namespace,

		kubeClient:    kubeClient,
		lhClient:      lhClient,
		eventRecorder: eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "longhorn-replica-controller"}),

		podControl: controller.RealPodControl{
			KubeClient: kubeClient,
			Recorder:   eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "longhorn-replica-controller"}),
		},
		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "longhorn-replica"),
	}

	replicaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			r := obj.(*longhorn.Replica)
			logrus.Debug("Add replica %s", r.Name)
			rc.enqueueReplicaHandler(r)
		},
		UpdateFunc: func(old, cur interface{}) {
			oldR := old.(*longhorn.Replica)
			curR := cur.(*longhorn.Replica)
			logrus.Debug("Update replica %s", oldR.Name)
			rc.enqueueReplicaHandler(curR)
		},
		DeleteFunc: func(obj interface{}) {
			r := obj.(*longhorn.Replica)
			logrus.Debug("Delete replica %s", r.Name)
			rc.enqueueReplicaHandler(r)
		},
	})
	rc.rLister = replicaInformer.Lister()
	rc.rStoreSynced = replicaInformer.Informer().HasSynced

	rc.pLister = podInformer.Lister()
	rc.pStoreSynced = podInformer.Informer().HasSynced

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    rc.addPod,
		UpdateFunc: rc.updatePod,
		DeleteFunc: rc.deletePod,
	})

	rc.syncHandler = rc.syncReplica
	rc.enqueueReplicaHandler = rc.enqueueReplica
	rc.updateReplicaHandler = rc.updateReplica
	return rc
}

func (rc *ReplicaController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer rc.queue.ShutDown()

	logrus.Infof("Start Longhorn replica controller")
	defer logrus.Infof("Shutting down Longhorn replica controller")

	if !controller.WaitForCacheSync("longhorn replicas", stopCh, rc.rStoreSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(rc.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (rc *ReplicaController) worker() {
	for rc.processNextWorkItem() {
	}
}

func (rc *ReplicaController) processNextWorkItem() bool {
	key, quit := rc.queue.Get()

	if quit {
		return false
	}
	defer rc.queue.Done(key)

	err := rc.syncHandler(key.(string))
	rc.handleErr(err, key)

	return true
}

func (rc *ReplicaController) handleErr(err error, key interface{}) {
	if err == nil {
		rc.queue.Forget(key)
		return
	}

	if rc.queue.NumRequeues(key) < maxRetries {
		logrus.Warnf("Error syncing Longhorn replica %v: %v", key, err)
		rc.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	logrus.Warnf("Dropping Longhorn replica %v out of the queue: %v", key, err)
	rc.queue.Forget(key)
}

func (rc *ReplicaController) syncReplica(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	if namespace != rc.namespace {
		// Not ours, don't do anything
		return nil
	}

	replicaRO, err := rc.rLister.Replicas(rc.namespace).Get(name)
	if apierrors.IsNotFound(err) {
		logrus.Infof("Longhorn replica %v has been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}

	replica := replicaRO.DeepCopy()

	// sync up with pod status
	pod, err := rc.pLister.Pods(rc.namespace).Get(replica.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			replica.Status.State = types.InstanceStateStopped
		} else {
			return err
		}
	} else {
		switch pod.Status.Phase {
		case v1.PodPending:
			replica.Status.State = types.InstanceStateStopped
		case v1.PodRunning:
			replica.Status.State = types.InstanceStateRunning
		default:
			logrus.Warnf("volume %v replica %v instance state is failed/unknown, pod state %v",
				replica.Spec.VolumeName, replica.Name, pod.Status.Phase)
			replica.Status.State = types.InstanceStateUnknown
		}
	}

	if replica.Spec.FailedAt != "" && replica.Spec.DesireState != types.InstanceStateStopped {
		replica.Spec.DesireState = types.InstanceStateStopped
		_, err := rc.updateReplicaHandler(replica)
		if err != nil {
			return err
		}
		rc.enqueueReplicaHandler(replica)
		return nil
	}
	if replica.DeletionTimestamp != nil && replica.Spec.DesireState != types.InstanceStateDeleted {
		replica.Spec.DesireState = types.InstanceStateDeleted
		_, err := rc.updateReplicaHandler(replica)
		if err != nil {
			return err
		}
		rc.enqueueReplicaHandler(replica)
		return nil
	}

	state := replica.Status.State
	desireState := replica.Spec.DesireState
	if desireState == types.InstanceStateDeleted && state == desireState {
		return rc.deleteReplica(replica)
	}

	if state != desireState {
		switch desireState {
		case types.InstanceStateRunning:
			if state == types.InstanceStateStopped {
				if err := rc.startReplicaInstance(replica); err != nil {
					return err
				}
				break
			}
			logrus.Errorf("unknown replica transition: current %v, desire %v", state, desireState)
		case types.InstanceStateStopped:
			if state == types.InstanceStateRunning {
				if err := rc.stopReplicaInstance(replica); err != nil {
					return err
				}
				break
			}
			logrus.Errorf("unknown replica transition: current %v, desire %v", state, desireState)
		case types.InstanceStateDeleted:
			if state == types.InstanceStateRunning {
				if err := rc.stopReplicaInstance(replica); err != nil {
					return err
				}
			}
			if state == types.InstanceStateStopped {
				if err := rc.cleanupReplicaInstance(replica); err != nil {
					return err
				}
				break
			}
			logrus.Errorf("unable to delete replica due to unknown state %v", state)
		default:
			logrus.Errorf("unknown replica transition: current %v, desire %v", state, desireState)
		}
	}
	return nil
}

func (rc *ReplicaController) enqueueReplica(replica *longhorn.Replica) {
	key, err := controller.KeyFunc(replica)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", replica, err))
		return
	}

	rc.queue.AddRateLimited(key)
}

func (rc *ReplicaController) updateReplica(r *longhorn.Replica) (*longhorn.Replica, error) {
	return rc.lhClient.LonghornV1alpha1().Replicas(rc.namespace).Update(r)
}

func (rc *ReplicaController) deleteReplica(r *longhorn.Replica) error {
	name := r.Name
	result, err := rc.rLister.Replicas(r.Namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errors.Wrapf(err, "unable to get replica during replica deletion %v", name)
	}
	resultCopy := result.DeepCopy()
	// Remove the finalizer to allow deletion of the object
	resultCopy.Finalizers = []string{}
	result, err = rc.updateReplicaHandler(result)
	if err != nil {
		return errors.Wrapf(err, "unable to update finalizer during replica deletion %v", name)
	}
	// No previous deletion operation, so we need to do it ourselves
	if result.DeletionTimestamp == nil {
		if err := rc.lhClient.LonghornV1alpha1().Replicas(rc.namespace).Delete(name,
			&metav1.DeleteOptions{}); err != nil {
			return errors.Wrapf(err, "unable to delete replica %v", name)
		}
	}

	return nil
}

func (rc *ReplicaController) getReplicaVolumeDirectory(replicaName string) string {
	return longhornDirectory + "/replicas/" + replicaName
}

func (rc *ReplicaController) createPodTemplateSpec(r *longhorn.Replica) *v1.PodTemplateSpec {
	cmd := []string{
		"launch", "replica",
		"--listen", "0.0.0.0:9502",
		"--size", r.Spec.VolumeSize,
	}
	if r.Spec.RestoreFrom != "" && r.Spec.RestoreName != "" {
		cmd = append(cmd, "--restore-from", r.Spec.RestoreFrom, "--restore-name", r.Spec.RestoreName)
	}
	cmd = append(cmd, "/volume")

	privilege := true
	pod := &v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.Name,
			Labels: map[string]string{
				longhornReplicaKey: r.Spec.VolumeName,
			},
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			Containers: []v1.Container{
				{
					Name:    r.Name,
					Image:   r.Spec.EngineImage,
					Command: cmd,
					SecurityContext: &v1.SecurityContext{
						Privileged: &privilege,
					},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "volume",
							MountPath: "/volume",
						},
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: "volume",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: rc.getReplicaVolumeDirectory(r.Name),
						},
					},
				},
			},
		},
	}

	// We will allow kubernetes to schedule it for the first time, later we
	// will pin it down to the same host because we have data on it
	if r.Spec.NodeID != "" {
		pod.Spec.NodeName = r.Spec.NodeID
	} else {
		pod.Spec.Affinity = &v1.Affinity{
			PodAntiAffinity: &v1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []v1.WeightedPodAffinityTerm{
					{
						Weight: 100,
						PodAffinityTerm: v1.PodAffinityTerm{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									longhornReplicaKey: r.Spec.VolumeName,
								},
							},
							TopologyKey: "kubernetes.io/hostname",
						},
					},
				},
			},
		}
	}
	return pod
}

func (rc *ReplicaController) createCleanupJobSpec(r *longhorn.Replica) *batchv1.Job {
	cmd := []string{"/bin/bash", "-c"}
	// There is a delay between starting pod and mount the volume, so
	// workaround it for now
	args := []string{"sleep 1 && rm -f /volume/*"}

	jobName := r.Name
	backoffLimit := int32(1)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            jobName,
			Namespace:       r.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(r, controllerKind)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cleanup-" + r.Name,
				},
				Spec: v1.PodSpec{
					NodeName:      r.Spec.NodeID,
					RestartPolicy: v1.RestartPolicyNever,
					Containers: []v1.Container{
						{
							Name:    "cleanup-" + r.Name,
							Image:   r.Spec.EngineImage,
							Command: cmd,
							Args:    args,
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      "volume",
									MountPath: "/volume",
								},
							},
						},
					},
					Volumes: []v1.Volume{
						{
							Name: "volume",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: rc.getReplicaVolumeDirectory(r.Name),
								},
							},
						},
					},
				},
			},
		},
	}
	return job
}

func (rc *ReplicaController) startReplicaInstance(r *longhorn.Replica) (err error) {
	podSpec := rc.createPodTemplateSpec(r)

	logrus.Debugf("Starting replica %v for %v", r.Name, r.Spec.VolumeName)
	if err := rc.podControl.CreatePodsWithControllerRef(rc.namespace, podSpec, r, metav1.NewControllerRef(r, controllerKind)); err != nil {
		return err
	}
	return nil
}

func (rc *ReplicaController) stopReplicaInstance(r *longhorn.Replica) (err error) {
	logrus.Debugf("Stopping replica %v for %v", r.Name, r.Spec.VolumeName)
	if err := rc.podControl.DeletePod(rc.namespace, r.Name, r); err != nil {
		return err
	}
	return nil
}

func (rc *ReplicaController) cleanupReplicaInstance(r *longhorn.Replica) (err error) {
	// replica wasn't created once, doesn't need clean up
	if r.Spec.NodeID == "" {
		return nil
	}
	job, err := rc.kubeClient.BatchV1().Jobs(rc.namespace).Get(r.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if job == nil {
		job := rc.createCleanupJobSpec(r)

		_, err = rc.kubeClient.BatchV1().Jobs(rc.namespace).Create(job)
		if err != nil {
			return errors.Wrap(err, "failed to create cleanup job")
		}
	} else {
		if job.Status.CompletionTime != nil {
			defer func() {
				err := rc.kubeClient.BatchV1().Jobs(rc.namespace).Delete(r.Name, &metav1.DeleteOptions{})
				if err != nil {
					logrus.Warnf("Failed to delete the cleanup job for %v: %v", r.Name, err)
				}
			}()

			if job.Status.Succeeded != 0 {
				logrus.Infof("Cleanup for volume %v replica %v succeed", r.Spec.VolumeName, r.Name)
				r.Status.State = types.InstanceStateDeleted
				if _, err := rc.updateReplicaHandler(r); err != nil {
					return err
				}
			} else {
				logrus.Warnf("Cleanup for volume %v replica %v failed", r.Spec.VolumeName, r.Name)
			}
			rc.enqueueReplicaHandler(r)
		}
	}

	return nil
}

func (rc *ReplicaController) addPod(obj interface{}) {
	pod := obj.(*v1.Pod)
	if controllerRef := metav1.GetControllerOf(pod); controllerRef != nil {
		replica := rc.resolveControllerRef(pod.Namespace, controllerRef)
		if replica == nil {
			return
		}
		rc.enqueueReplicaHandler(replica)
		return
	}
}

func (rc *ReplicaController) updatePod(old, cur interface{}) {
	curPod := cur.(*v1.Pod)
	oldPod := old.(*v1.Pod)
	if curPod.ResourceVersion == oldPod.ResourceVersion {
		// Periodic resync will send update events for all known pods.
		// Two different versions of the same pod will always have
		// different RVs.
		return
	}
	if curPod.DeletionTimestamp != nil {
		// when a pod is deleted gracefully it's deletion timestamp is
		// first modified to reflect a grace period, and after such time
		// has passed, the kubelet actually deletes it from the store.
		// We receive an update for modification of the deletion
		// timestamp and expect to operate on it ASAP, not until the
		// kubelet actually deletes the pod.
		rc.deletePod(curPod)
		return
	}

	curControllerRef := metav1.GetControllerOf(curPod)
	// Only deal with replica belonged to this controller at this time
	replica := rc.resolveControllerRef(curPod.Name, curControllerRef)
	if replica == nil {
		return
	}
	rc.enqueueReplicaHandler(replica)
}

func (rc *ReplicaController) deletePod(obj interface{}) {
	pod, ok := obj.(*v1.Pod)

	if !ok {
		utilruntime.HandleError(fmt.Errorf("couldn't get object %+v", obj))
		return
	}

	controllerRef := metav1.GetControllerOf(pod)
	if controllerRef == nil {
		// No controller should care about orphans being deleted.
		return
	}
	replica := rc.resolveControllerRef(pod.Namespace, controllerRef)
	if replica == nil {
		return
	}
	rc.enqueueReplicaHandler(replica)
}

// resolveControllerRef returns the controller referenced by a ControllerRef,
// or nil if the ControllerRef could not be resolved to a matching controller
// of the correct Kind.
func (rc *ReplicaController) resolveControllerRef(namespace string, controllerRef *metav1.OwnerReference) *longhorn.Replica {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != controllerKind.Kind {
		return nil
	}
	replica, err := rc.rLister.Replicas(namespace).Get(controllerRef.Name)
	if err != nil {
		return nil
	}
	if replica.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return replica
}
