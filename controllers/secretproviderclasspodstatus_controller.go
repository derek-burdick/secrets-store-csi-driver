/*
Copyright 2020 The Kubernetes Authors.

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
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"k8s.io/client-go/tools/record"

	log "github.com/sirupsen/logrus"

	"sigs.k8s.io/secrets-store-csi-driver/apis/v1alpha1"
	"sigs.k8s.io/secrets-store-csi-driver/pkg/client/clientset/versioned/scheme"
	"sigs.k8s.io/secrets-store-csi-driver/pkg/util/fileutil"
	"sigs.k8s.io/secrets-store-csi-driver/pkg/util/secretutil"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	secretManagedLabel         = "secrets-store.csi.k8s.io/managed"
	secretCreationFailedReason = "FailedToCreateSecret"
)

// SecretProviderClassPodStatusReconciler reconciles a SecretProviderClassPodStatus object
type SecretProviderClassPodStatusReconciler struct {
	client.Client
	mutex         *sync.Mutex
	log           *log.Logger
	scheme        *apiruntime.Scheme
	nodeID        string
	reader        client.Reader
	writer        client.Writer
	eventRecorder record.EventRecorder
}

// New creates a new SecretProviderClassPodStatusReconciler
func New(mgr manager.Manager, nodeID string) (*SecretProviderClassPodStatusReconciler, error) {
	eventBroadcaster := record.NewBroadcaster()
	kubeClient := kubernetes.NewForConfigOrDie(mgr.GetConfig())
	eventBroadcaster.StartRecordingToSink(&clientcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "csi-secrets-store-controller"})

	return &SecretProviderClassPodStatusReconciler{
		Client:        mgr.GetClient(),
		mutex:         &sync.Mutex{},
		scheme:        mgr.GetScheme(),
		nodeID:        nodeID,
		reader:        mgr.GetCache(),
		writer:        mgr.GetClient(),
		eventRecorder: recorder,
	}, nil
}

func (r *SecretProviderClassPodStatusReconciler) RunPatcher(stopCh <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			if err := r.Patcher(); err != nil {
				log.Errorf("failed to patch secret owner ref, err: %+v", err)
			}
		}
	}
}

func (r *SecretProviderClassPodStatusReconciler) Patcher() error {
	log.Debugf("patcher started")
	r.mutex.Lock()
	defer r.mutex.Unlock()

	ctx := context.Background()
	spcPodStatusList := &v1alpha1.SecretProviderClassPodStatusList{}
	spcMap := make(map[string]v1alpha1.SecretProviderClass)
	secretOwnerMap := make(map[types.NamespacedName][]*v1alpha1.SecretProviderClassPodStatus)
	// get a list of all spc pod status that belong to the node
	err := r.reader.List(ctx, spcPodStatusList, r.ListOptionsLabelSelector())
	if err != nil {
		return fmt.Errorf("failed to list secret provider class pod status, err: %+v", err)
	}

	spcPodStatuses := spcPodStatusList.Items
	for i := range spcPodStatuses {
		spcName := spcPodStatuses[i].Status.SecretProviderClassName
		spc := &v1alpha1.SecretProviderClass{}
		if val, exists := spcMap[spcPodStatuses[i].Namespace+"/"+spcName]; exists {
			spc = &val
		} else {
			if err := r.reader.Get(ctx, client.ObjectKey{Namespace: spcPodStatuses[i].Namespace, Name: spcName}, spc); err != nil {
				log.Errorf("failed to get spc %s, err: %+v", spcName, err)
				return err
			}
			spcMap[spcPodStatuses[i].Namespace+"/"+spcName] = *spc
		}
		for _, secret := range spc.Spec.SecretObjects {
			key := types.NamespacedName{Name: secret.SecretName, Namespace: spcPodStatuses[i].Namespace}
			val, exists := secretOwnerMap[key]
			if exists {
				secretOwnerMap[key] = append(val, &spcPodStatuses[i])
			} else {
				secretOwnerMap[key] = []*v1alpha1.SecretProviderClassPodStatus{&spcPodStatuses[i]}
			}
		}
	}

	for secret, owners := range secretOwnerMap {
		patchFn := func() (bool, error) {
			if err := r.patchSecretWithOwnerRef(ctx, secret.Name, secret.Namespace, owners...); err != nil {
				if !apierrors.IsConflict(err) || !apierrors.IsTimeout(err) {
					log.Errorf("failed to set owner ref for secret, err: %+v", err)
				}
				return false, nil
			}
			return true, nil
		}
		if err := wait.ExponentialBackoff(wait.Backoff{
			Steps:    5,
			Duration: 1 * time.Millisecond,
			Factor:   1.0,
			Jitter:   0.1,
		}, patchFn); err != nil {
			return err
		}
	}

	log.Debugf("patcher completed")
	return nil
}

// ListOptionsLabelSelector returns a ListOptions with a label selector for node name.
func (r *SecretProviderClassPodStatusReconciler) ListOptionsLabelSelector() client.ListOption {
	return client.MatchingLabels(map[string]string{
		v1alpha1.InternalNodeLabel: r.nodeID,
	})
}

// +kubebuilder:rbac:groups=secrets-store.csi.x-k8s.io,resources=secretproviderclasspodstatuses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=secrets-store.csi.x-k8s.io,resources=secretproviderclasspodstatuses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=secrets-store.csi.x-k8s.io,resources=secretproviderclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *SecretProviderClassPodStatusReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	ctx := context.Background()
	logger := log.WithFields(log.Fields{"secretproviderclasspodstatus": req.NamespacedName, "node": r.nodeID})
	logger.Info("reconcile started")

	var spcPodStatus v1alpha1.SecretProviderClassPodStatus
	if err := r.reader.Get(ctx, req.NamespacedName, &spcPodStatus); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Errorf("failed to get spc pod status, err: %+v", err)
		return ctrl.Result{}, err
	}

	// reconcile delete
	if !spcPodStatus.GetDeletionTimestamp().IsZero() {
		logger.Infof("reconcile complete")
		return ctrl.Result{}, nil
	}

	node, ok := spcPodStatus.GetLabels()[v1alpha1.InternalNodeLabel]
	if !ok {
		logger.Info("node label not found, ignoring this spc pod status")
		return ctrl.Result{}, nil
	}
	if !strings.EqualFold(node, r.nodeID) {
		logger.Infof("ignoring as spc pod status belongs to node %s", node)
		return ctrl.Result{}, nil
	}

	spcName := spcPodStatus.Status.SecretProviderClassName
	spc := &v1alpha1.SecretProviderClass{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: spcName}, spc); err != nil {
		logger.Errorf("failed to get spc %s, err: %+v", spcName, err)
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	if len(spc.Spec.SecretObjects) == 0 {
		logger.Infof("no secret objects defined for spc, nothing to reconcile")
		return ctrl.Result{}, nil
	}

	// podObjectReference is an object reference to the pod that spc pod status
	// is created for. The object reference is created with minimal required fields
	// name, namespace and UID. By doing this we can skip an additional client call
	// to fetch the pod object
	podObjectReference, err := getPodObjectReference(spcPodStatus)
	if err != nil {
		logger.Errorf("failed to get pod object reference, error: %+v", err)
	}

	files, err := fileutil.GetMountedFiles(spcPodStatus.Status.TargetPath)
	if err != nil {
		r.generateEvent(podObjectReference, corev1.EventTypeWarning, secretCreationFailedReason, fmt.Sprintf("failed to get mounted files, err: %+v", err))
		logger.Errorf("failed to get mounted files, err: %+v", err)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, err
	}
	errs := make([]error, 0)
	for _, secretObj := range spc.Spec.SecretObjects {
		secretName := strings.TrimSpace(secretObj.SecretName)

		if err = secretutil.ValidateSecretObject(*secretObj); err != nil {
			logger.Errorf("failed to validate secret object in spc %s/%s, err: %+v", spc.Namespace, spc.Name, err)
			errs = append(errs, fmt.Errorf("failed to validate secret object in spc %s/%s, err: %+v", spc.Namespace, spc.Name, err))
			continue
		}
		exists, err := r.secretExists(ctx, secretName, req.Namespace)
		if err != nil {
			logger.Errorf("failed to check if secret %s exists, err: %+v", secretName, err)
			errs = append(errs, fmt.Errorf("failed to check if secret %s exists, err: %+v", secretName, err))
			continue
		}

		var funcs []func() (bool, error)

		if !exists {
			secretType := secretutil.GetSecretType(strings.TrimSpace(secretObj.Type))

			datamap := make(map[string][]byte)
			if datamap, err = secretutil.GetSecretData(secretObj.Data, secretType, files); err != nil {
				r.generateEvent(podObjectReference, corev1.EventTypeWarning, secretCreationFailedReason, fmt.Sprintf("failed to get data in spc %s/%s for secret %s, err: %+v", req.Namespace, spcName, secretName, err))
				log.Errorf("failed to get data in spc %s/%s for secret %s, err: %+v", req.Namespace, spcName, secretName, err)
				errs = append(errs, fmt.Errorf("failed to get data in spc %s/%s for secret %s, err: %+v", req.Namespace, spcName, secretName, err))
				continue
			}

			labelsMap := make(map[string]string)
			if secretObj.Labels != nil {
				labelsMap = secretObj.Labels
			}
			// Set secrets-store.csi.k8s.io/managed=true label on the secret that's created and managed
			// by the secrets-store-csi-driver. This label will be used to perform a filtered list watch
			// only on secrets created and managed by the driver
			labelsMap[secretManagedLabel] = "true"

			createFn := func() (bool, error) {
				if err := r.createK8sSecret(ctx, secretName, req.Namespace, datamap, labelsMap, secretType); err != nil {
					logger.Errorf("failed createK8sSecret, err: %v for secret: %s", err, secretName)
					return false, nil
				}
				return true, nil
			}
			funcs = append(funcs, createFn)
		}

		for _, f := range funcs {
			if err := wait.ExponentialBackoff(wait.Backoff{
				Steps:    5,
				Duration: 1 * time.Millisecond,
				Factor:   1.0,
				Jitter:   0.1,
			}, f); err != nil {
				r.generateEvent(podObjectReference, corev1.EventTypeWarning, secretCreationFailedReason, err.Error())
				return ctrl.Result{RequeueAfter: 5 * time.Second}, err
			}
		}
	}

	if len(errs) > 0 {
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("reconcile complete")
	// requeue the spc pod status again after 5mins to check if secret and ownerRef exists
	// and haven't been modified. If secret doesn't exist, then this requeue will ensure it's
	// created in the next reconcile and the owner ref patched again
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *SecretProviderClassPodStatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SecretProviderClassPodStatus{}).
		Complete(r)
}

// createK8sSecret creates K8s secret with data from mounted files
// If a secret with the same name already exists in the namespace of the pod, the error is nil.
func (r *SecretProviderClassPodStatusReconciler) createK8sSecret(ctx context.Context, name, namespace string, datamap map[string][]byte, labelsmap map[string]string, secretType corev1.SecretType) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    labelsmap,
		},
		Type: secretType,
		Data: datamap,
	}

	err := r.writer.Create(ctx, secret)
	if err == nil {
		log.Infof("created k8s secret: %s/%s", namespace, name)
		return nil
	}
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// patchSecretWithOwnerRef patches the secret owner reference with the spc pod status
func (r *SecretProviderClassPodStatusReconciler) patchSecretWithOwnerRef(ctx context.Context, name, namespace string, spcPodStatus ...*v1alpha1.SecretProviderClassPodStatus) error {
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}
	if err := r.Client.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			log.Debugf("secret %s/%s not found for patching", namespace, name)
			return nil
		}
		return err
	}

	patch := client.MergeFromWithOptions(secret.DeepCopy(), client.MergeFromWithOptimisticLock{})
	needsPatch := false

	secretOwnerMap := make(map[string]types.UID)
	for _, or := range secret.GetOwnerReferences() {
		secretOwnerMap[or.Name] = or.UID
	}

	for i := range spcPodStatus {
		if _, exists := secretOwnerMap[spcPodStatus[i].Name]; exists {
			continue
		}
		needsPatch = true
		err := controllerutil.SetOwnerReference(spcPodStatus[i], secret, r.scheme)
		if err != nil {
			return err
		}
	}

	if needsPatch {
		return r.writer.Patch(ctx, secret, patch)
	}
	return nil
}

// secretExists checks if the secret with name and namespace already exists
func (r *SecretProviderClassPodStatusReconciler) secretExists(ctx context.Context, name, namespace string) (bool, error) {
	o := &v1.Secret{}
	secretKey := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}
	err := r.Client.Get(ctx, secretKey, o)
	if err == nil {
		return true, nil
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return false, err
}

// getPodObjectReference returns a v1.ObjectReference for the pod object
func getPodObjectReference(spcPodStatus v1alpha1.SecretProviderClassPodStatus) (*v1.ObjectReference, error) {
	podName := spcPodStatus.Status.PodName
	podNamespace := spcPodStatus.Namespace
	podUID := getPodUIDFromTargetPath(spcPodStatus.Status.TargetPath)
	if podUID == "" {
		return nil, fmt.Errorf("failed to get pod UID from target path")
	}
	return &v1.ObjectReference{
		Name:      podName,
		Namespace: podNamespace,
		UID:       types.UID(podUID),
	}, nil
}

// getPodUIDFromTargetPath returns podUID from targetPath
func getPodUIDFromTargetPath(targetPath string) string {
	re := regexp.MustCompile(`[\\|\/]+pods[\\|\/]+(.+?)[\\|\/]+volumes`)
	match := re.FindStringSubmatch(targetPath)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

// generateEvent generates an event
func (r *SecretProviderClassPodStatusReconciler) generateEvent(obj runtime.Object, eventType, reason, message string) {
	if obj != nil {
		r.eventRecorder.Eventf(obj, eventType, reason, message)
	}
}
