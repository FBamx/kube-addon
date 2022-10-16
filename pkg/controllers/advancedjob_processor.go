package controllers

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/util/retry"
	v1helper "k8s.io/component-helpers/scheduling/corev1"
	v1affinityhelper "k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"
	kubecontroller "k8s.io/kubernetes/pkg/controller"
	daemonsetutil "k8s.io/kubernetes/pkg/controller/daemon/util"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodename"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeunschedulable"
	"k8s.io/utils/integer"
	v1 "kube-addon/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sync"
	"time"
)

func podLabelAboutAdvancedJob(job *v1.AdvancedJob) map[string]string {
	return map[string]string{
		AdvancedJobNameLabelKey: job.Name,
		ControllerUIDLabelKey:   string(job.UID),
	}
}

func addLabelToPodTemplate(job *v1.AdvancedJob) {
	if job.Spec.Template.Labels == nil {
		job.Spec.Template.Labels = make(map[string]string)
	}
	for k, v := range podLabelAboutAdvancedJob(job) {
		job.Spec.Template.Labels[k] = v
	}
}

func IsJobFinished(job *v1.AdvancedJob) bool {
	if job.Spec.CompletionPolicy.Type == v1.Never {
		return false
	}

	for _, c := range job.Status.Conditions {
		if (c.Type == v1.JobComplete || c.Type == v1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func pastTTLDeadline(job *v1.AdvancedJob) (bool, time.Duration) {
	if job.Spec.CompletionPolicy.TTLSecondsAfterFinished == nil || job.Status.CompletionTime == nil {
		return false, -1
	}
	now := metav1.Now()
	finishTime := job.Status.CompletionTime.Time
	duration := now.Time.Sub(finishTime)
	allowedDuration := time.Duration(*job.Spec.CompletionPolicy.TTLSecondsAfterFinished) * time.Second
	return duration >= allowedDuration, allowedDuration - duration
}

func (r *AdvancedJobReconciler) getNodeToPodMap(pods []*corev1.Pod, job *v1.AdvancedJob) map[string]*corev1.Pod {
	nodeToPodMap := make(map[string]*corev1.Pod)
	for i, pod := range pods {
		nodeName := pod.Spec.NodeName
		if _, ok := nodeToPodMap[pod.Spec.NodeName]; ok {
			klog.Errorf("Duplicated pod %s run on the same node %s. this should not happen.", pod.Name, nodeName)
			r.Recorder.Eventf(job, corev1.EventTypeWarning, "DuplicatePodCreatedOnSameNode",
				"Duplicated pod %s found on same node %s", pod.Name, nodeName)
		}
		nodeToPodMap[nodeName] = pods[i]
	}
	return nodeToPodMap
}

func asOwner(job *v1.AdvancedJob) *metav1.OwnerReference {
	return metav1.NewControllerRef(job, controllerKind)
}

func (r *AdvancedJobReconciler) createPod(nodeName, namespace string, template *corev1.PodTemplateSpec, object runtime.Object,
	controllerRef *metav1.OwnerReference) error {
	pod, err := kubecontroller.GetPodFromTemplate(template, object, controllerRef)
	if err != nil {
		return err
	}
	pod.Namespace = namespace
	pod.Spec.Affinity = daemonsetutil.ReplaceDaemonSetPodNodeNameNodeAffinity(pod.Spec.Affinity, nodeName)
	if err := r.Client.Create(context.TODO(), pod); err != nil {
		r.Recorder.Eventf(object, corev1.EventTypeWarning, kubecontroller.FailedCreatePodReason, "Error creating: %v", err)
		return err
	}
	return nil
}

// isPodFailed marks the pod as a failed pod, when
// 1. restartPolicy==Never, and exit code is not 0
// 2. restartPolicy==OnFailure, and RestartCount > restartLimit
func isPodFailed(restartLimit int32, pod *corev1.Pod) bool {
	if pod.Spec.RestartPolicy != corev1.RestartPolicyOnFailure {
		return false
	}

	restartCount := int32(0)
	for i := range pod.Status.InitContainerStatuses {
		stat := pod.Status.InitContainerStatuses[i]
		restartCount += stat.RestartCount
	}
	for i := range pod.Status.ContainerStatuses {
		stat := pod.Status.ContainerStatuses[i]
		restartCount += stat.RestartCount
	}

	return restartCount > restartLimit
}

// filterPods returns list of activePods and number of failed pods, number of succeeded pods
func filterPods(restartLimit int32, pods []*corev1.Pod) ([]*corev1.Pod, []*corev1.Pod, []*corev1.Pod) {
	var activePods, succeededPods, failedPods []*corev1.Pod
	for _, p := range pods {
		if p.Status.Phase == corev1.PodSucceeded {
			succeededPods = append(succeededPods, p)
		} else if p.Status.Phase == corev1.PodFailed {
			failedPods = append(failedPods, p)
		} else if p.DeletionTimestamp == nil {
			if isPodFailed(restartLimit, p) {
				failedPods = append(failedPods, p)
			} else {
				activePods = append(activePods, p)
			}
		} else {
			klog.V(4).Infof("Ignoring inactive pod %v/%v in state %v, deletion time %v",
				p.Namespace, p.Name, p.Status.Phase, p.DeletionTimestamp)
		}
	}
	return activePods, failedPods, succeededPods
}

func logPredicateFailedReason(node *corev1.Node, status *framework.Status) (bool, error) {
	if status.IsSuccess() {
		return true, nil
	}
	for _, reason := range status.Reasons() {
		klog.Errorf("Failed predicate on node %s : %s ", node.Name, reason)
	}
	return status.IsSuccess(), status.AsError()
}

// checkNodeFitness runs a set of predicates that select candidate nodes for the job pod;
// the predicates include:
//   - PodFitsHost: checks pod's NodeName against node
//   - PodMatchNodeSelector: checks pod's ImagePullJobNodeSelector and NodeAffinity against node
//   - PodToleratesNodeTaints: exclude tainted node unless pod has specific toleration
//   - CheckNodeUnschedulablePredicate: check if the pod can tolerate node unschedulable
//   - PodFitsResources: checks if a node has sufficient resources, such as cpu, memory, gpu, opaque int resources etc to run a pod.
func checkNodeFitness(pod *corev1.Pod, node *corev1.Node) (bool, error) {
	nodeInfo := framework.NewNodeInfo()
	nodeInfo.SetNode(node)

	if len(pod.Spec.NodeName) != 0 && pod.Spec.NodeName != node.Name {
		return logPredicateFailedReason(node, framework.NewStatus(framework.UnschedulableAndUnresolvable, nodename.ErrReason))
	}

	if fitsNodeAffinity, _ := v1affinityhelper.GetRequiredNodeAffinity(pod).Match(node); !fitsNodeAffinity {
		return logPredicateFailedReason(node, framework.NewStatus(framework.UnschedulableAndUnresolvable, nodeaffinity.ErrReasonPod))
	}

	filterPredicate := func(t *corev1.Taint) bool {
		// PodToleratesNodeTaints is only interested in NoSchedule and NoExecute taints.
		return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
	}
	taint, isUntolerated := v1helper.FindMatchingUntoleratedTaint(node.Spec.Taints, pod.Spec.Tolerations, filterPredicate)
	if isUntolerated {
		errReason := fmt.Sprintf("node(s) had taint {%s: %s}, that the pod didn't tolerate",
			taint.Key, taint.Value)
		return logPredicateFailedReason(node, framework.NewStatus(framework.UnschedulableAndUnresolvable, errReason))
	}

	// If pod tolerate unschedulable taint, it's also tolerate `node.Spec.Unschedulable`.
	podToleratesUnschedulable := v1helper.TolerationsTolerateTaint(pod.Spec.Tolerations, &corev1.Taint{
		Key:    corev1.TaintNodeUnschedulable,
		Effect: corev1.TaintEffectNoSchedule,
	})
	if nodeInfo.Node().Spec.Unschedulable && !podToleratesUnschedulable {
		return logPredicateFailedReason(node, framework.NewStatus(framework.UnschedulableAndUnresolvable, nodeunschedulable.ErrReasonUnschedulable))
	}

	insufficientResources := noderesources.Fits(pod, nodeInfo, true)
	if len(insufficientResources) != 0 {
		// We will keep all failure reasons.
		failureReasons := make([]string, 0, len(insufficientResources))
		for _, r := range insufficientResources {
			failureReasons = append(failureReasons, r.Reason)
		}
		return logPredicateFailedReason(node, framework.NewStatus(framework.Unschedulable, failureReasons...))
	}

	return true, nil
}

// NewMockPod creates a new mock pod
func NewMockPod(job *v1.AdvancedJob, nodeName string) *corev1.Pod {
	newPod := &corev1.Pod{Spec: job.Spec.Template.Spec, ObjectMeta: job.Spec.Template.ObjectMeta}
	newPod.Namespace = job.Namespace
	newPod.Spec.NodeName = nodeName
	return newPod
}

// getNodesToRunPod returns
// * desiredNodes : the nodes desired to run pods including node with or without running pods
// * restNodesToRunPod:  the nodes do not have pods running yet, excluding the nodes not satisfying constraints such as affinity, taints
// * podsToDelete: the pods that do not satisfy the node constraint any more
func getNodesToRunPod(nodes *corev1.NodeList, job *v1.AdvancedJob,
	existingNodeToPodMap map[string]*corev1.Pod) (map[string]*corev1.Pod, []*corev1.Node, []*corev1.Pod) {

	var podsToDelete []*corev1.Pod
	var restNodesToRunPod []*corev1.Node
	desiredNodes := make(map[string]*corev1.Pod)
	for i, node := range nodes.Items {

		var canFit bool
		var err error
		// there's pod existing on the node
		if pod, ok := existingNodeToPodMap[node.Name]; ok {
			canFit, err = checkNodeFitness(pod, &node)
			if err != nil {
				klog.Errorf("pod %s failed to checkNodeFitness for node %s, %v", pod.Name, node.Name, err)
				continue
			}
			if !canFit {
				if pod.DeletionTimestamp == nil {
					podsToDelete = append(podsToDelete, pod)
				}
				continue
			}
			desiredNodes[node.Name] = pod
		} else {
			// no pod exists, mock a pod to check if the pod can fit on the node,
			// considering nodeName, label affinity and taints
			mockPod := NewMockPod(job, node.Name)
			canFit, err = checkNodeFitness(mockPod, &node)
			if err != nil {
				klog.Errorf("failed to checkNodeFitness for node %s, %v", node.Name, err)
				continue
			}
			if !canFit {
				klog.Infof("Pod does not fit on node %s", node.Name)
				continue
			}
			restNodesToRunPod = append(restNodesToRunPod, &nodes.Items[i])
			desiredNodes[node.Name] = nil
		}
	}
	return desiredNodes, restNodesToRunPod, podsToDelete
}

func (r *AdvancedJobReconciler) updateJobStatus(request reconcile.Request, job *v1.AdvancedJob) error {
	klog.Infof("Updating job %s status %#v", job.Name, job.Status)
	jobCopy := job.DeepCopy()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := r.Status().Update(context.TODO(), jobCopy)
		if err == nil {
			return nil
		}

		updated := &v1.AdvancedJob{}
		err = r.Get(context.TODO(), request.NamespacedName, updated)
		if err == nil {
			jobCopy = updated
			jobCopy.Status = job.Status
		} else {
			utilruntime.HandleError(fmt.Errorf("error getting updated AdvancedJob %s/%s from lister: %v", job.Namespace, job.Name, err))
		}
		return err
	})
}

// deleteJobPods delete the pods concurrently and wait for them to be done
func (r *AdvancedJobReconciler) deleteJobPods(job *v1.AdvancedJob, pods []*corev1.Pod, failed, active int32) (int32, int32, error) {
	errCh := make(chan error, len(pods))
	wait := sync.WaitGroup{}
	nbPods := len(pods)
	var failedLock sync.Mutex
	wait.Add(nbPods)
	for i := int32(0); i < int32(nbPods); i++ {
		go func(ix int32) {
			defer wait.Done()
			if err := r.Delete(context.TODO(), pods[ix]); err != nil {
				defer utilruntime.HandleError(err)
				klog.Infof("Failed to delete %v, job %q/%q", pods[ix].Name, job.Namespace, job.Name)
				errCh <- err
			} else {
				failedLock.Lock()
				failed++
				active--
				r.Recorder.Eventf(job, corev1.EventTypeNormal, kubecontroller.SuccessfulDeletePodReason, "Delete pod: %v", pods[ix].Name)
				failedLock.Unlock()
			}
		}(i)
	}
	wait.Wait()
	var manageJobErr error
	select {
	case manageJobErr = <-errCh:
	default:
	}
	return failed, active, manageJobErr
}

func newCondition(conditionType v1.JobConditionType, reason, message string) v1.JobCondition {
	return v1.JobCondition{
		Type:               conditionType,
		Status:             corev1.ConditionTrue,
		LastProbeTime:      metav1.Now(),
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
}

// finishJob appends the condition to JobStatus, and sets ttl if needed
func finishJob(job *v1.AdvancedJob, conditionType v1.JobConditionType, message string) time.Duration {
	job.Status.Conditions = append(job.Status.Conditions, newCondition(conditionType, string(conditionType), message))
	klog.Infof("job %s/%s is %s: %s", job.Namespace, job.Name, string(conditionType), message)

	now := metav1.Now()
	job.Status.CompletionTime = &now

	var requeueAfter time.Duration
	if job.Spec.CompletionPolicy.TTLSecondsAfterFinished != nil {
		klog.Infof("Job %s is %s, will be deleted after %d seconds", job.Name, string(conditionType),
			*job.Spec.CompletionPolicy.TTLSecondsAfterFinished)
		// a bit more than the TTLSecondsAfterFinished to ensure it exceeds the TTLSecondsAfterFinished when being reconciled
		requeueAfter = time.Duration(*job.Spec.CompletionPolicy.TTLSecondsAfterFinished+1) * time.Second
	}
	return requeueAfter
}

func validateControllerRef(controllerRef *metav1.OwnerReference) error {
	if controllerRef == nil {
		return fmt.Errorf("controllerRef is nil")
	}
	if len(controllerRef.APIVersion) == 0 {
		return fmt.Errorf("controllerRef has empty APIVersion")
	}
	if len(controllerRef.Kind) == 0 {
		return fmt.Errorf("controllerRef has empty Kind")
	}
	if controllerRef.Controller == nil || *controllerRef.Controller != true {
		return fmt.Errorf("controllerRef.Controller is not set to true")
	}
	if controllerRef.BlockOwnerDeletion == nil || *controllerRef.BlockOwnerDeletion != true {
		return fmt.Errorf("controllerRef.BlockOwnerDeletion is not set")
	}
	return nil
}

func (r *AdvancedJobReconciler) createPodOnNode(nodeName, namespace string, template *corev1.PodTemplateSpec, object runtime.Object, controllerRef *metav1.OwnerReference) error {
	if err := validateControllerRef(controllerRef); err != nil {
		return err
	}
	return r.createPod(nodeName, namespace, template, object, controllerRef)
}

func (r *AdvancedJobReconciler) reconcilePods(job *v1.AdvancedJob,
	restNodesToRunPod []*corev1.Node, active, desired int32) (int32, error) {

	// max concurrent running pods
	var parallelism int32
	var err error
	parallelismInt, err := intstr.GetValueFromIntOrPercent(intstr.ValueOrDefault(job.Spec.Parallelism, intstr.FromInt(1<<31-1)), int(desired), true)
	if err != nil {
		return active, err
	}
	parallelism = int32(parallelismInt)

	// The rest pods to run
	rest := int32(len(restNodesToRunPod))
	var errCh chan error
	if active > parallelism {
		// exceed parallelism limit
		r.Recorder.Eventf(job, corev1.EventTypeWarning, "TooManyActivePods", "Number of active pods exceed parallelism limit")
		//TODO should we remove the extra pods ? it may just finish by its own.

	} else if active < parallelism {
		// diff is the current number of pods to run in this reconcile loop
		diff := integer.Int32Min(parallelism-active, rest)

		// Batch the pod creates. Batch sizes start at SlowStartInitialBatchSize
		// and double with each successful iteration in a kind of "slow start".
		// This handles attempts to start large numbers of pods that would
		// likely all fail with the same error. For example a project with a
		// low quota that attempts to create a large number of pods will be
		// prevented from spamming the API service with the pod create requests
		// after one of its pods fails.  Conveniently, this also prevents the
		// event spam that those failures would generate.
		var activeLock sync.Mutex
		errCh = make(chan error, diff)
		wait := sync.WaitGroup{}
		startIndex := int32(0)
		for batchSize := integer.Int32Min(diff, kubecontroller.SlowStartInitialBatchSize); diff > 0; batchSize = integer.Int32Min(2*batchSize, diff) {
			// count of errors in current error channel
			errorCount := len(errCh)
			wait.Add(int(batchSize))

			// create pod concurrently in each batch by go routine
			curBatchNodes := restNodesToRunPod[startIndex : startIndex+batchSize]
			for _, node := range curBatchNodes {
				go func(nodeName string) {
					defer wait.Done()
					// parallelize pod creation
					klog.Infof("creating pod on node %s", nodeName)
					err := r.createPodOnNode(nodeName, job.Namespace, &job.Spec.Template, job, asOwner(job))
					if err != nil && errors.IsTimeout(err) {
						// Pod is created but its initialization has timed out.
						// If the initialization is successful eventually, the
						// controller will observe the creation via the informer.
						// If the initialization fails, or if the pod keeps
						// uninitialized for a long time, the informer will not
						// receive any update, and the controller will create a new
						// pod when the expectation expires.
						return
					}
					if err != nil {
						defer utilruntime.HandleError(err)
						errCh <- err
					}
					// If succeed, increase active counter
					activeLock.Lock()
					active++
					activeLock.Unlock()
				}(node.Name)
			}
			// wait for all pods created
			wait.Wait()
			// If there are error occurs and there are still pods remaining to be created
			skippedPods := diff - batchSize
			if errorCount < len(errCh) && skippedPods > 0 {
				// The skipped pods will be retried later. The next controller resync will
				// retry the slow start process.
				break
			}
			diff -= batchSize
			startIndex += batchSize
		}
	}
	select {
	case err = <-errCh:
	default:
	}
	return active, err
}

// isJobComplete returns true if all pods on all desiredNodes are either succeeded or failed or deletionTimestamp !=nil.
func isJobComplete(job *v1.AdvancedJob, desiredNodes map[string]*corev1.Pod) bool {
	if job.Spec.CompletionPolicy.Type == v1.Never {
		// the job will not terminate, if the the completion policy is never
		return false
	}
	// if no desiredNodes, job pending
	if len(desiredNodes) == 0 {
		klog.Info("Num desiredNodes is 0")
		return false
	}
	for _, pod := range desiredNodes {
		if pod == nil || kubecontroller.IsPodActive(pod) {
			// the job is incomplete if there exits any pod not yet created OR  still active
			return false
		}
	}
	return true
}

// pastActiveDeadline checks if job has ActiveDeadlineSeconds field set and if it is exceeded.
func pastActiveDeadline(job *v1.AdvancedJob) bool {
	if job.Spec.CompletionPolicy.ActiveDeadlineSeconds == nil || job.Status.StartTime == nil {
		return false
	}
	now := metav1.Now()
	start := job.Status.StartTime.Time
	duration := now.Time.Sub(start)
	allowedDuration := time.Duration(*job.Spec.CompletionPolicy.ActiveDeadlineSeconds) * time.Second
	return duration >= allowedDuration
}

// isJobFailed checks if the job CompletionPolicy is not Never, and it has past the backofflimit or ActiveDeadlineSeconds.
func isJobFailed(job *v1.AdvancedJob, pods []*corev1.Pod) (bool, string, string) {
	if job.Spec.CompletionPolicy.Type == v1.Never {
		return false, "", ""
	}
	jobFailed := false
	var failureReason string
	var failureMessage string
	if pastActiveDeadline(job) {
		jobFailed = true
		failureReason = "DeadlineExceeded"
		failureMessage = fmt.Sprintf("Job %s/%s was active longer than specified deadline", job.Namespace, job.Name)
	}
	return jobFailed, failureReason, failureMessage
}
