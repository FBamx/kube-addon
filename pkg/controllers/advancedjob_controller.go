/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"flag"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"kube-addon/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"time"
)

//+kubebuilder:rbac:groups=apps.addon.io,resources=advancedjobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps.addon.io,resources=advancedjobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps.addon.io,resources=advancedjobs/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete

func init() {
	flag.BoolVar(&scheduleBroadcastJobPods, "assign-bcj-pods-by-scheduler", true, "Use scheduler to assign broadcastJob pod to node.")
}

const (
	AdvancedJobNameLabelKey = "broadcastjob-name"
	ControllerUIDLabelKey   = "broadcastjob-controller-uid"
)

var (
	concurrentReconciles     = 3
	scheduleBroadcastJobPods bool
	controllerKind           = v1.GroupVersion.WithKind("BroadcastJob")
)

// AdvancedJobReconciler reconciles a AdvancedJob object
type AdvancedJobReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// podModifier is only for testing to set the pod.Name, if pod.GenerateName is used
	podModifier func(pod *corev1.Pod)
}

// SetupWithManager sets up the controller with the Manager.
func (r *AdvancedJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.AdvancedJob{}).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(event event.UpdateEvent) bool {
				oldObj := event.ObjectOld
				newObj := event.ObjectNew
				return oldObj.GetGeneration() != newObj.GetGeneration()
			},
		}).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrentReconciles}).
		Watches(&source.Kind{Type: &corev1.Pod{}}, &podEventHandler{client: mgr.GetClient()}).
		Watches(&source.Kind{Type: &corev1.Node{}}, &nodeEventHandler{client: mgr.GetClient()}).
		Complete(r)
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *AdvancedJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)
	job := &v1.AdvancedJob{}
	err := r.Get(context.TODO(), req.NamespacedName, job)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	addLabelToPodTemplate(job)

	if IsJobFinished(job) {
		if isPast, leftTime := pastTTLDeadline(job); isPast {
			klog.Infof("deleting the job %s", job.Name)
			err = r.Delete(context.TODO(), job)
			if err != nil {
				klog.Errorf("failed to delete job %s", job.Name)
			}
		} else if leftTime > 0 {
			return reconcile.Result{RequeueAfter: leftTime}, nil
		}
		return reconcile.Result{}, nil
	}

	requeueAfter := time.Duration(0)

	if job.Status.StartTime == nil {
		now := metav1.Now()
		job.Status.StartTime = &now
		if job.Spec.CompletionPolicy.Type == v1.Always &&
			job.Spec.CompletionPolicy.ActiveDeadlineSeconds != nil {
			klog.Infof("Job %s has ActiveDeadlineSeconds, will resync after %d seconds",
				job.Name, *job.Spec.CompletionPolicy.ActiveDeadlineSeconds)
			requeueAfter = time.Duration(*job.Spec.CompletionPolicy.ActiveDeadlineSeconds) * time.Second
		}
	}

	if job.Status.Phase == "" {
		job.Status.Phase = v1.PhaseRunning
	}

	// list pods for this job
	podList := &corev1.PodList{}
	if err := r.List(context.TODO(), podList, &client.ListOptions{
		Namespace:     req.Namespace,
		LabelSelector: labels.SelectorFromSet(podLabelAboutAdvancedJob(job)),
	}); err != nil {
		klog.Errorf("failed to get podList for job %s", job.Name)
		return ctrl.Result{}, err
	}

	var pods []*corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		controllerRef := metav1.GetControllerOf(pod)
		if controllerRef != nil && controllerRef.Kind == job.Kind && controllerRef.UID == job.UID {
			pods = append(pods, pod)
		}
	}

	existingNodeToPodMap := r.getNodeToPodMap(pods, job)
	nodeList := &corev1.NodeList{}
	if err := r.List(context.TODO(), nodeList); err != nil {
		klog.Errorf("failed to get nodeList for Job %s", job.Name)
		return ctrl.Result{}, err
	}

	// Get active, failed, succeeded pods
	activePods, failedPods, succeededPods := filterPods(job.Spec.FailurePolicy.RestartLimit, pods)
	active := int32(len(activePods))
	failed := int32(len(failedPods))
	succeeded := int32(len(succeededPods))

	var desired int32
	desiredNodes, restNodesToRunPod, podsToDelete := getNodesToRunPod(nodeList, job, existingNodeToPodMap)
	desired = int32(len(desiredNodes))
	klog.Infof("%s/%s has %d/%d nodes remaining to schedule pods", job.Namespace, job.Name, len(restNodesToRunPod), desired)
	klog.Infof("Before broadcastjob reconcile %s/%s, desired=%d, active=%d, failed=%d", job.Namespace, job.Name, desired, active, failed)
	job.Status.Active = active
	job.Status.Failed = failed
	job.Status.Succeeded = succeeded
	job.Status.Desired = desired

	if job.Status.Phase == v1.PhaseFailed {
		return reconcile.Result{RequeueAfter: requeueAfter}, r.updateJobStatus(req, job)
	}

	if job.Spec.Paused && (job.Status.Phase == v1.PhaseRunning || job.Status.Phase == v1.PhasePaused) {
		job.Status.Phase = v1.PhasePaused
		return reconcile.Result{RequeueAfter: requeueAfter}, r.updateJobStatus(req, job)
	}
	if !job.Spec.Paused && job.Status.Phase == v1.PhasePaused {
		job.Status.Phase = v1.PhaseRunning
		r.Recorder.Event(job, corev1.EventTypeNormal, "Continue", "continue to process job")
	}

	jobFailed := false
	var failureReason, failureMessage string
	if failed > 0 {
		switch job.Spec.FailurePolicy.Type {
		case v1.FailurePolicyTypePause:
			r.Recorder.Event(job, corev1.EventTypeWarning, "Paused", "job is paused, due to failed pod")
			job.Spec.Paused = true
			job.Status.Phase = v1.PhasePaused
			return reconcile.Result{RequeueAfter: requeueAfter}, r.updateJobStatus(req, job)
		case v1.FailurePolicyTypeFailFast:
			// mark the job is failed
			jobFailed, failureReason, failureMessage = true, "failed pod is found", "failure policy is FailurePolicyTypeFailFast and failed pod is found"
			r.Recorder.Event(job, corev1.EventTypeWarning, failureReason, fmt.Sprintf("%s: %d pods succeeded, %d pods failed", failureMessage, succeeded, failed))
		case v1.FailurePolicyTypeContinue:
		}
	}

	if !jobFailed {
		jobFailed, failureReason, failureMessage = isJobFailed(job, pods)
	}
	// Job is failed. For keepAlive type, the job will never fail.
	if jobFailed {
		// Handle Job failures, delete all active pods
		failed, active, err = r.deleteJobPods(job, activePods, failed, active)
		if err != nil {
			klog.Errorf("failed to deleteJobPods for job %s,", job.Name)
		}
		job.Status.Phase = v1.PhaseFailed
		requeueAfter = finishJob(job, v1.JobFailed, failureMessage)
		r.Recorder.Event(job, corev1.EventTypeWarning, failureReason,
			fmt.Sprintf("%s: %d pods succeeded, %d pods failed", failureMessage, succeeded, failed))
	} else {
		// Job is still active
		if len(podsToDelete) > 0 {
			//should we remove the pods without nodes associated, the podgc controller will do this if enabled
			failed, active, err = r.deleteJobPods(job, podsToDelete, failed, active)
			if err != nil {
				klog.Errorf("failed to deleteJobPods for job %s,", job.Name)
			}
		}

		// DeletionTimestamp is not set and more nodes to run pod
		if job.DeletionTimestamp == nil && len(restNodesToRunPod) > 0 {
			active, err = r.reconcilePods(job, restNodesToRunPod, active, desired)
			if err != nil {
				klog.Errorf("failed to reconcilePods for job %s,", job.Name)
			}
		}

		if isJobComplete(job, desiredNodes) {
			message := fmt.Sprintf("Job completed, %d pods succeeded, %d pods failed", succeeded, failed)
			job.Status.Phase = v1.PhaseCompleted
			requeueAfter = finishJob(job, v1.JobComplete, message)
			r.Recorder.Event(job, corev1.EventTypeNormal, "JobComplete",
				fmt.Sprintf("Job %s/%s is completed, %d pods succeeded, %d pods failed", job.Namespace, job.Name, succeeded, failed))
		}
	}
	klog.Infof("After broadcastjob reconcile %s/%s, desired=%d, active=%d, failed=%d", job.Namespace, job.Name, desired, active, failed)

	// update the status
	job.Status.Failed = failed
	job.Status.Active = active
	if err := r.updateJobStatus(req, job); err != nil {
		klog.Errorf("failed to update job %s, %v", job.Name, err)
	}

	return reconcile.Result{RequeueAfter: requeueAfter}, err
}
